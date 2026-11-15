// Package pacing gives a protocol run a human rhythm.
//
// The rationale is measured, not stylistic: a pure-HTTP registration walks
// bootstrap → preflight → captcha → validate in about 2.5 seconds with five
// requests, while the real widget takes 90-150 seconds and dozens of requests.
// "Fast and clean" is itself a signal, so the executor can space its stages.
//
// Two rules keep this honest:
//   - the delay happens *before* a request is sent, never after a side effect,
//     so a crash cannot leave Apple with a half-applied action plus silence;
//   - the duration is derived deterministically from (seed, from, to), so a
//     resumed task reproduces the same rhythm and tests do not wait.
package pacing

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"
)

// Config is frozen with the task so a resume keeps the same rhythm.
type Config struct {
	Enabled bool          `json:"enabled"`
	Min     time.Duration `json:"min"`
	Max     time.Duration `json:"max"`
	Seed    int64         `json:"seed"`
}

// Default is what the CLI uses when -human is on and nothing else is given.
func Default(seed int64) Config {
	return Config{Enabled: true, Min: 1500 * time.Millisecond, Max: 9000 * time.Millisecond, Seed: seed}
}

// Pacer spaces requests. Sleep is injectable so tests run at full speed.
type Pacer struct {
	cfg   Config
	sleep func(context.Context, time.Duration) error
	// applied records what was actually waited, for the receipt.
	applied []string
}

// New builds a pacer. A nil sleep means "really wait".
func New(cfg Config, sleep func(context.Context, time.Duration) error) *Pacer {
	if sleep == nil {
		sleep = realSleep
	}
	return &Pacer{cfg: cfg, sleep: sleep}
}

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Duration is the deterministic wait for one edge of the flow. Same seed and
// same edge always produce the same value, which is what makes a resumed task
// indistinguishable from the original run.
func (p *Pacer) Duration(from, to string) time.Duration {
	if !p.cfg.Enabled {
		return 0
	}
	span := p.cfg.Max - p.cfg.Min
	if span <= 0 {
		return p.cfg.Min
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s|%s", p.cfg.Seed, from, to)
	return p.cfg.Min + time.Duration(h.Sum64()%uint64(span))
}

// Before waits for the edge into the given stage. It returns the applied delay
// so callers can record it.
func (p *Pacer) Before(ctx context.Context, from, to string) (time.Duration, error) {
	d := p.Duration(from, to)
	if d <= 0 {
		return 0, nil
	}
	if err := p.sleep(ctx, d); err != nil {
		return 0, err
	}
	p.applied = append(p.applied, fmt.Sprintf("%s->%s=%dms", from, to, d.Milliseconds()))
	return d, nil
}

// Applied is the receipt form: one entry per waited edge.
func (p *Pacer) Applied() []string {
	out := make([]string, len(p.applied))
	copy(out, p.applied)
	return out
}

// Enabled reports whether this pacer is doing anything at all.
func (p *Pacer) Enabled() bool { return p.cfg.Enabled }
