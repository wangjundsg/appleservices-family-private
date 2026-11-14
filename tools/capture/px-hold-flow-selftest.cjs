// Offline control-flow test for the PX hold logic in outlook-cn.cjs.
//
// Pulls the REAL clearChallenge() source and drives it against a fake page whose
// mouse records every event, so the four rules the operator asked for are
// asserted mechanically:
//   1. after mouse.down() there is NO mouse.move before mouse.up()
//   2. exactly ONE frame probe happens inside each hold
//   3. no screenshot fires inside a hold (shotsPaused holds the whole time)
//   4. round 1 = static hold, round 2 = 800 ms tap + 800 ms gap + >=8 s hold,
//      then it stops -- never a third submission on the same exit
//
// Run: node tools/capture/px-hold-flow-selftest.cjs
'use strict';

const fs = require('node:fs');
const path = require('node:path');

const SOURCE = fs.readFileSync(path.join(__dirname, 'outlook-cn.cjs'), 'utf8');
const CLEAR_SRC = (() => {
  const a = SOURCE.indexOf('async function clearChallenge');
  const b = SOURCE.indexOf('// --------------------------------------------------------------- artefacts');
  if (a < 0 || b < 0) throw new Error('clearChallenge not found');
  return SOURCE.slice(a, b);
})();
const HOLD_SRC = SOURCE.match(/const holdMsFixed = \(\(\) => \{[\s\S]*?\}\)\(\);/)[0];

function makeRunner(scanScript, opts = {}) {
  const state = { events: [], probes: [], shots: [], paused: [] };
  const mouse = {
    move(x, y) { state.events.push({ kind: 'move', x, y }); },
    down() { state.events.push({ kind: 'down' }); },
    up() { state.events.push({ kind: 'up' }); },
  };
  const page = { mouse };
  const kinds = scanScript.slice();
  const deps = {
    holdMsFixed: opts.hold_ms || 7000,
    r2: (n) => Math.round(Number(n) * 10) / 10,
    sleep: (ms) => new Promise((r) => setTimeout(r, Math.min(ms, opts.cap_sleep || 4))),
    rand: (a, b) => (a + b) / 2,
    mouseWander: async () => { state.events.push({ kind: 'wander' }); },
    log: (step, detail) => state.events.push({ kind: 'log', step, detail }),
    screenshotNow: async (p, name) => { state.shots.push(name); },
    glideTo: async (p, pt) => { state.events.push({ kind: 'glide', pt }); },
    scanChallenge: async () => {
      const kind = kinds.length > 1 ? kinds.shift() : kinds[0];
      return { kind, rows: [], pressFrame: '', pressBox: null };
    },
    locatePressTarget: async () => ({
      frame: { evaluate: async () => ({ text: '按住 •••', has_px: true }) },
      frameUrl: 'https://iframe.hsprotect.net/index.html?app_id=X',
      selector: '#px-captcha#0',
      capsulePage: { x: 338, y: 542, w: 360, h: 42 },
      point: { x: 610, y: 563 },
      fallbackPoint: { x: 518, y: 563 },
      textNodeBox: { x: 596, y: 553, w: 28, h: 20 },
      textNodeCenterFrame: { x: 610, y: 563 },
      frameOrigin: { x: 338, y: 542 },
      text: '按住', exact: true, score: 2, labels_seen: 2, frameText: '按住 •••',
    }),
    checkPressPoint: async () => ({
      in_view: true, viewport: { w: 1036, h: 798 },
      hit: { tag: 'IFRAME', src: 'iframe.hsprotect.net/x' }, hit_hsprotect: true, ok: true,
    }),
    holdProbeOnce: async () => { state.probes.push(Date.now()); return { took_ms: 3, text: '按住 •••', has_px: true }; },
  };
  const body = `
    var shotsPaused = false;
    ${CLEAR_SRC.replace(/shotsPaused = (true|false);/g, (m, v) => `shotsPaused = ${v}; PAUSED.push(${v});`)}
    return { clearChallenge };
  `;
  const PAUSED = [];
  const fn = new Function(...Object.keys(deps), 'PAUSED', body);
  const api = fn(...Object.values(deps), PAUSED);
  state.paused = PAUSED;
  return { api, state, page };
}

let fails = 0;
function assert(name, ok, extra) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${extra === undefined ? '' : '  ' + JSON.stringify(extra)}`);
  if (!ok) fails += 1;
}

(async () => {
  // ---- scenario 1: never clears -> both rounds run, then stop --------------
  {
    const { api, state, page } = makeRunner(['press-visible'], { hold_ms: 7000 });
    const res = await api.clearChallenge(page, 2);
    const seq = state.events.filter((e) => ['down', 'up', 'move', 'glide', 'wander'].includes(e.kind)).map((e) => e.kind);
    const downs = seq.reduce((acc, k, i) => (k === 'down' ? acc.concat(i) : acc), []);
    const pairs = downs.map((d) => {
      const u = seq.indexOf('up', d);
      return { down: d, up: u, between: seq.slice(d + 1, u) };
    });

    // round 1 = one press; round 2 = the 800 ms tap + the long hold = 3 in total
    assert('three presses total (hold, tap, hold)', pairs.length === 3 && pairs.every((p) => p.up > p.down), seq);
    assert('zero pointer movement in every down->up window', pairs.every((p) => p.between.length === 0), pairs.map((p) => p.between));
    assert('exactly one frame probe per round', state.probes.length === 2, { probes: state.probes.length });
    assert('only before/after screenshots (none inside a hold)', state.shots.every((n) => /^press-(before|after)-r\d$/.test(n)), state.shots);
    assert('shotsPaused toggles 6x around the holds', JSON.stringify(state.paused) === JSON.stringify([true, false, true, false, true, false]), state.paused);

    const starts = state.events.filter((e) => e.kind === 'log' && e.step === 'press_start').map((e) => ({ round: e.detail.round, mode: e.detail.mode, hold_ms: e.detail.hold_ms }));
    assert('plan = still 7000 then tap-then-still 8000',
      starts.length === 2 && starts[0].mode === 'still' && starts[0].hold_ms === 7000
      && starts[1].mode === 'tap-then-still' && starts[1].hold_ms === 8000, starts);
    const tap = state.events.find((e) => e.kind === 'log' && e.step === 'press_tap_done');
    assert('round 2 does 800 ms tap + 800 ms gap before the hold', Boolean(tap) && tap.detail.tap_ms === 800 && tap.detail.gap_ms === 800, tap && tap.detail);

    const rows = (res.attempts_log || []).map((r) => ({ round: r.round, mode: r.mode, tap_ms: r.tap_ms }));
    assert('attempts log: still then tap-then-still', rows.length === 2 && rows[0].mode === 'still' && rows[1].mode === 'tap-then-still' && rows[1].tap_ms === 800, rows);
    assert('terminal: not passed, two rounds, named note', res.passed === false && res.attempts === 2 && /still challenged/.test(res.note || ''), { passed: res.passed, attempts: res.attempts, note: res.note });
    assert('mid-hold probe logged at 1500ms for both rounds', (res.attempts_log || []).every((r) => r.mid && r.mid.at_ms === 1500 && r.mid.has_px === true), (res.attempts_log || []).map((r) => r.mid));
    assert('frame text captured before and during the hold', (res.attempts_log || []).every((r) => r.text_before === '按住 •••' && r.text_during === '按住 •••' && r.state_changed_during_hold === false), (res.attempts_log || []).map((r) => [r.text_before, r.text_during, r.state_changed_during_hold]));
  }

  // ---- scenario 2: clears after round 1 -> stop immediately ----------------
  {
    const { api, state, page } = makeRunner(['press-visible', 'press-visible', 'none'], { hold_ms: 7000 });
    const res = await api.clearChallenge(page, 2);
    assert('cleared after round 1 -> passed with 1 attempt', res.passed === true && res.attempts === 1, { passed: res.passed, attempts: res.attempts });
    assert('round 2 never started (one press only)', state.events.filter((e) => e.kind === 'down').length === 1, state.events.filter((e) => e.kind === 'down').length);
  }

  // ---- scenario 3: hold-ms clamps into the 6000-8000 band ------------------
  {
    const run = (argVal) => {
      const argv = argVal === null ? [] : ['--hold-ms', String(argVal)];
      const f = new Function('argv', `const arg = (name, fb='') => { const i = argv.indexOf('--'+name); return i >= 0 && argv[i+1] ? argv[i+1] : fb; };\n${HOLD_SRC}\nreturn holdMsFixed;`);
      return f(argv);
    };
    assert('default hold = 7000', run(null) === 7000, run(null));
    assert('clamped low  3000 -> 6000', run(3000) === 6000, run(3000));
    assert('clamped high 9000 -> 8000', run(9000) === 8000, run(9000));
    assert('in-band 6500 kept', run(6500) === 6500, run(6500));
  }

  console.log(fails === 0 ? '\nALL PASS' : `\n${fails} FAILURE(S)`);
  process.exit(fails === 0 ? 0 : 1);
})();
