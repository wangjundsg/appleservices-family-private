// Run with playwright-cli run-code against an isolated dashboard. All API
// calls are intercepted; no Apple account or purchase is touched.
async page => {
  const assert = (value, message) => { if (!value) throw Error(message); };
  let paid = false, role = 'organizer';
  const calls = [], errors = []; let job=null;
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/api/**', async route => {
    const path = new URL(route.request().url()).pathname;
    let value = {ok:true};
    if(path==='/api/view') value={selected:'fixture-child',accounts:[{id:'fixture-child',name:'测试账号',icloud_activated:true},{id:'fixture-mother',name:'测试母号',icloud_activated:true,family_organizer:true}],status:{running:true,authenticated:true},icloud_activated:true,control_ready:true,devices:[],cn_family_job:job};
    else if(path==='/api/register/config') value={familyOrganizers:[]};
    else if(path==='/api/register') value={jobs:[]};
    else if(path==='/api/cn-family-background') {
      const body=route.request().postDataJSON();calls.push(body.step);
      assert(body.organizer==='fixture-mother'&&body.account==='fixture-child','wrong queued binding');
      assert(body.accept_terms&&body.auto_future&&!('password' in body),'missing consent or persisted password');
      job={account:'fixture-child',organizer:'fixture-mother',state:'completed',step:'family-verify'};
    }
    else if(path==='/api/cn-family') {
      const body = route.request().postDataJSON();
      calls.push(body.step);
      assert(body.account==='fixture-child' && body.role===role,'account or role changed during run');
      if(role==='member') assert(body.organizer==='fixture-mother','wrong organizer');
      assert(!('password' in body),'empty password leaked into request');
      if(body.step==='observe') value={ok:true,online:true,activated:true};
      else if(body.step==='activate') value={ok:true,activation:{completed:true}};
      else if(body.step==='subscription-plan') value=paid?{ok:true,existing:true}:{ok:true,purchase_action:'storage-skill-purchase-cn'};
    }
    await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify(value)});
  });
  await page.reload();
  await page.locator('#cnFamilyRole').selectOption('organizer');
  await page.locator('#cnFamilyTerms').check();
  await page.locator('#cnFamilyRun').click();
  await page.locator('#cnFamilyProgress').filter({hasText:'等待你完成'}).waitFor();
  assert(!calls.includes('subscription-cancel')&&!calls.includes('family-create'),'payment wait performed writes');
  paid=true;calls.length=0;
  await page.locator('#cnFamilyRun').click();
  await page.locator('#cnFamilyProgress').filter({hasText:'母号准备完成'}).waitFor();
  assert(calls.indexOf('subscription-cancel')<calls.indexOf('family-create'),'cancel not before sharing');
  role='member';calls.length=0;
  await page.locator('#cnFamilyRole').selectOption(role);
  await page.locator('#cnFamilyOrganizer').selectOption('fixture-mother');
  await page.locator('#cnFamilyRun').click();
  await page.locator('#cnFamilyProgress').filter({hasText:'子号完成'}).waitFor();
  assert(!calls.some(x=>x.startsWith('subscription')),'member invoked commerce');
  assert(calls.includes('enqueue')&&!calls.includes('family-join'),'browser still owns member writes');
  await page.reload();await page.locator('#cnFamilyProgress').filter({hasText:'子号完成'}).waitFor();
  assert(errors.length===0,errors.join('\n'));
  return {passed:true,scenarios:3};
}
