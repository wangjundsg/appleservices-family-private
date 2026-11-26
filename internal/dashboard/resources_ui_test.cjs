// Run with playwright-cli run-code --filename=... against an isolated panel only.
// Registration POST is blocked even if the fixture or UI changes.
async page => {
  const assert=(value,message)=>{if(!value)throw Error(message);};
  let starts=0;
  await page.route('**/api/register',async route=>{
    if(route.request().method()==='POST'){starts++;await route.abort();}else await route.continue();
  });
  await page.reload();
  await page.locator('#resourceWorkbench summary').click();
  const form=page.locator('#resourceForm');
  await page.waitForFunction(()=>document.getElementById('resourceSMSState').textContent.includes('已加载'));
  assert((await page.locator('#resourceMailState').innerText()).includes('绑定'),'missing order not explained');
  assert((await page.locator('#resourcePurchaseState').innerText()).includes('关闭'),'isolated server must disable purchasing');
  for(const kind of ['mail','sms','proxy']) await page.locator(`[data-resource-check=${kind}]`).click();
  for(const id of ['resourceMailResult','resourceSMSResult','resourceProxyResult']) await page.locator('#'+id+'[data-ok=true]').waitFor({timeout:60000});
  assert((await page.locator('#resourceMailResult').innerText()).includes('尚未绑定'),'unbound order falsely ready');
  assert(/活跃订单 \d+/.test(await page.locator('#resourceSMSResult').innerText()),'actual active orders not displayed');
  assert((await page.locator('#resourceProxyResult').innerText()).includes('未核验实际出口地区'),'connectivity overclaimed country');
  await page.locator('#registerToggle').click();
  assert(await page.locator('#registerSubmit').isDisabled(),'new registration enabled without order/purchase switch');
  assert(await page.locator('#registerForm [name=phonecc]').inputValue()==='PL','saved country not reflected');
  assert(await page.locator('#registerForm [name=phonecc]').isDisabled(),'automatic country can diverge from settings');
  await page.locator('#registerToggle').click();
  await form.locator('[name=maxPrice]').fill('0.20');
  await form.locator('[name=maxTotal]').fill('0.40');
  await form.locator('[name=durationHours]').selectOption('0');
  assert(await page.locator('#resourceRentalConsent').isHidden(),'rental consent shown for ordinary activation');
  await form.locator('[name=durationHours]').selectOption('24');
  assert(!(await form.locator('[name=acceptRentalPriceChange]').isChecked()),'rental price consent silently enabled');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(()=>document.getElementById('resourceNotice').textContent.includes('已保存'));
  await page.reload();await page.locator('#resourceWorkbench summary').click();
  await page.waitForFunction(()=>document.getElementById('resourceForm').elements.namedItem('maxPrice').value==='0.20');
  assert((await page.locator('#resourceMailResult').innerText()).includes('尚未查询'),'old live check claimed after reload');
  await page.route('**/api/register/resources/check',async route=>{
    await route.fulfill({status:200,contentType:'application/json',body:JSON.stringify({ok:false,resource:'sms',message:'Hero 余额查询被拒绝或响应无效',checkedAt:new Date().toISOString(),readOnly:true})});
  });
  await page.locator('[data-resource-check=sms]').click();
  await page.locator('#resourceSMSResult[data-ok=false]').waitFor();
  assert((await page.locator('#resourceSMSResult').innerText()).includes('被拒绝'),'provider failure hidden');
  await page.unroute('**/api/register/resources/check');
  await page.reload();await page.locator('#resourceWorkbench summary').click();
  await page.waitForFunction(()=>document.getElementById('resourceSMSState').textContent.includes('已加载'));
  await page.setViewportSize({width:1440,height:1100});
  await page.locator('#resourceWorkbench').screenshot({path:'.cache/resource-panel-check/resources-desktop.png'});
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'mobile overflow');
  await page.locator('#resourceWorkbench').screenshot({path:'.cache/resource-panel-check/resources-mobile.png'});
  assert(starts===0,'test attempted registration');
  return {ok:true,checks:['live provider UI','no purchase','persisted settings','explicit rental consent','visible provider error','mobile layout']};
}
