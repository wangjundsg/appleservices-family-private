// Run against an isolated panel with playwright-cli run-code --filename=... .
// Every registration request is intercepted; this test cannot spend a number.
async page => {
  const check = (value, message) => { if (!value) throw new Error(message); };
  let job = null, submitted = null, delivered = false, cancelled = false;
  await page.route('**/api/register**', async route => {
    const request = route.request(), url = new URL(request.url());
    let value;
    if (url.pathname === '/api/register' && request.method() === 'POST') {
      submitted = request.postDataJSON();
      job = {jobId:'reg-20261114-abcdefabcdefabcd',state:'waiting_input',stage:'creating',startedAt:new Date().toISOString(),elapsedMs:12000,active:true,canRetry:false};
      value = {jobId:job.jobId};
    } else if (url.pathname.endsWith('/input')) {
      delivered = request.postDataJSON().code === '654321'; value = {ok:true};
    } else if (url.pathname.endsWith('/cancel')) {
      cancelled = true; job = {...job,state:'cancelled',active:false}; value = {message:'已请求取消'};
    } else if (url.pathname.endsWith('/log')) {
      value = [{stage:'create',state:'waiting_input',at:new Date().toISOString()}];
    } else if (url.pathname.endsWith('/evidence')) { value = []; }
    else value = {jobs:job ? [job]:[]};
    await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(value)});
  });
  await page.reload();
  await page.locator('#registerToggle').click();
  await page.locator('#registerForm').waitFor({state:'visible'});
  check(await page.locator('.registration-card').evaluate(e => getComputedStyle(e).opacity === '1'),'registration card invisible');
  const form = page.locator('#registerForm');
  await form.locator('[name=email]').fill('fixture@example.test');
  await form.locator('[name=password]').fill('weak');
  check(await form.locator('[name=password]').evaluate(e => !e.checkValidity()),'weak password accepted');
  await form.locator('[name=password]').fill('Fixture-only123!');
  await form.locator('[name=phone]').fill('7700900123');
  await form.locator('[name=activation]').fill('invalid/activation');
  check(await form.locator('[name=activation]').evaluate(e => !e.checkValidity()),'invalid activation accepted');
  await form.locator('[name=activation]').fill('fixture-activation');
  const date = new Date(Date.now()+600000);
  const local = new Date(date.getTime()-date.getTimezoneOffset()*60000).toISOString().slice(0,16);
  await form.locator('[name=expiresAt]').fill(local);
  await form.locator('[name=mode]').selectOption('direct');
  check(await page.locator('#registerPortLabel').isHidden(),'direct exit still requires proxy port');
  await form.locator('[name=acceptTerms]').check();
  await page.setViewportSize({width:1280,height:960});
  await page.screenshot({path:'.cache/panel-qa/desktop.png'});
  await page.setViewportSize({width:390,height:844});
  check(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth),'mobile page overflows');
  await page.screenshot({path:'.cache/panel-qa/mobile.png'});
  await page.locator('#registerSubmit').click();
  await page.locator('#registerCodeForm').waitFor({state:'visible'});
  check(submitted && submitted.egress.mode==='direct' && !('platformKey' in submitted),'wrong registration contract');
  check(await form.locator('[name=password]').inputValue() === '', 'password retained after submission');
  await page.locator('#registerEmailCode').fill('654321');
  await page.locator('#registerCodeForm button').click();
  await page.waitForFunction(() => document.getElementById('registerNotice').textContent.includes('已交给'));
  check(delivered,'email code not delivered');
  check(await page.locator('#registerEmailCode').inputValue() === '', 'code retained after submission');
  check(await page.locator('#registerRetry').isHidden(),'unknown write can retry');
  await page.evaluate(() => { window.confirm = () => true; });
  await page.locator('#registerCancel').click();
  await page.waitForFunction(() => document.getElementById('registerState').textContent.includes('已取消'));
  check(cancelled,'cancel not submitted');
  job = {...job,state:'failed',canRetry:true};
  await page.locator('#registerRetry').waitFor({state:'visible'});
  await page.locator('#registerRetry').click();
  await form.waitFor({state:'visible'});
  check(await form.locator('[name=activation]').inputValue() === '', 'retry reused old activation');
  check(await form.locator('[name=password]').inputValue() === '', 'retry reused password');
  await page.unroute('**/api/register**');
  return {passed:true,checks:['desktop/mobile','password and activation validation','submit','email code','cancel','safe retry']};
}
