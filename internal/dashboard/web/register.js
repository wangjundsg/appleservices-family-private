(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const token = document.querySelector('meta[name="app-token"]').content;
  const form = $('registerForm');
  const field = name => form.elements.namedItem(name);
  const stages = ['creating','login','twofa','terms','device','keepalive','subscription','family'];
  const stageNames = {queued:'排队',creating:'注册',create:'注册',login:'交接面板',terms:'iCloud 激活',twofa:'账户验证',gsa:'账户验证',device:'设备登记',keepalive:'后台在线',subscription:'iCloud+ 订阅',family:'家庭共享',done:'完成'};
  const stateNames = {queued:'等待开始',running:'正在进行',waiting_input:'等待验证码',ready:'流程已完成',failed:'失败',usage:'参数需要修正',unknown:'结果待核实',needs_review:'需要核实',rate_limited:'Apple 要求冷却',needs_login:'会话需要重新验证',cancelled:'已取消'};
  let selected = '', retryOf = '', submitting = false, polling = false;
  let jobs = [];
  let setup = null;
  let familySelected=false;
  const resourceForm=$('resourceForm'), resourceField=name=>resourceForm.elements.namedItem(name);
  let resourceBusy=false, resourceChecking=0;
  function rentalConsent() { $('resourceRentalConsent').hidden=resourceField('durationHours').value!=='24'; }
  resourceField('durationHours').addEventListener('change',rentalConsent);
  function renderResources(view) {
    const settings=view.settings;
    for(const name of ['credentialFile','orderFile','phoneCountry','durationHours','proxyPort','localBrowserProxy']) resourceField(name).value=settings[name]|| (name==='durationHours'?'0':'');
    resourceField('acceptRentalPriceChange').checked=!!settings.acceptRentalPriceChange;
    resourceField('maxPrice').value=view.purchase.maxPrice||'';resourceField('maxTotal').value=view.purchase.maxTotal||'';
    resourceField('proxyProfile').replaceChildren(...view.proxyProfiles.map(p=>new Option(p.name,p.id)));
    resourceField('proxyProfile').value=settings.proxyProfile;
    if(!resourceField('proxyPort').value) resourceField('proxyPort').value=view.proxyPort||'';
    $('resourceMailState').textContent=view.orderConfigured ? '已绑定 Gmail 订单，仓库 '+view.warehouse+'；供应商状态待查询。' : view.orderMessage;
    $('resourceSMSState').textContent=view.smsCredentialLoaded ? 'Hero 密钥已加载；号码有效期与库存以供应商回执为准。' : '当前面板未加载 Hero 密钥，请使用配置好凭据的启动环境。';
    $('resourceProxyName').textContent=view.proxyProvider||'现有配置';
    $('resourceProxyState').textContent=view.proxyConfigured ? '配置已读取；按账号地区固定出口，手机国家独立。' : view.proxyMessage;
    $('resourcePurchaseState').textContent=view.purchaseEnabled ? '购号开关已开启；开始注册才会购号。' : '当前服务购号开关关闭；保存与查询不会开启。';
    if(view.configError) resourceNotice(view.configError);
    rentalConsent();
  }
  function resourceNotice(message) { $('resourceNotice').textContent=message;$('resourceNotice').hidden=!message; }
  async function loadResources() { renderResources(await api('/resources')); }
  resourceForm.addEventListener('submit',async event=>{
    event.preventDefault();if(resourceBusy||resourceChecking||!resourceForm.reportValidity())return;
    resourceBusy=true;const button=resourceForm.querySelector('button[type=submit]');button.disabled=true;
    try {
      const settings={mailProvider:'otpgmail',smsProvider:'hero',credentialFile:resourceField('credentialFile').value.trim(),orderFile:resourceField('orderFile').value.trim(),phoneCountry:resourceField('phoneCountry').value,durationHours:Number(resourceField('durationHours').value),acceptRentalPriceChange:resourceField('acceptRentalPriceChange').checked,proxyProfile:resourceField('proxyProfile').value,proxyPort:Number(resourceField('proxyPort').value)};
      settings.localBrowserProxy=resourceField('localBrowserProxy').value.trim();
      const purchase={maxPrice:resourceField('maxPrice').value.trim(),maxTotal:resourceField('maxTotal').value.trim(),currency:'USD'};
      renderResources(await api('/resources',{settings,purchase}));
      for(const id of ['resourceMailResult','resourceSMSResult','resourceProxyResult']) {$(id).textContent='设置已更改，请重新查询';delete $(id).dataset.ok;}
      resourceNotice('资源设置已保存。已有任务保持原配置；未购买或发送验证码。');
      await loadSetup();
    } catch(error) {resourceNotice(error.message);} finally {resourceBusy=false;button.disabled=false;}
  });
  for(const button of document.querySelectorAll('[data-resource-check]')) button.addEventListener('click',async()=>{
    if(resourceBusy)return;
    resourceChecking++;resourceForm.querySelector('button[type=submit]').disabled=true;
    const kind=button.dataset.resourceCheck, output=$({mail:'resourceMailResult',sms:'resourceSMSResult',proxy:'resourceProxyResult'}[kind]);
    button.disabled=true;output.textContent='正在按已保存的设置执行只读查询…';delete output.dataset.ok;
    try {
      const answer=await api('/resources/check',{resource:kind});
      output.dataset.ok=String(answer.ok);
      let message=answer.message;
      if(answer.ok) {
        const result=answer.result;
        if(kind==='mail') {
          const states={not_bound:'尚未绑定邮箱订单',waiting_code:'订单等待新码',queued:'订单仍在排队',completed:'订单已有完成回执，需核实原任务',cancelled:'订单已取消'};
          message='余额 '+result.balanceVnd+' VND。'+result.offers.map(o=>'仓库 '+o.warehouse+'：'+o.priceVnd+' VND，库存 '+o.stock).join('；')+'。'+(states[result.orderStatus]||'订单状态待核实')+'。取到验证码不等于账号创建成功。';
        } else if(kind==='sms') message='余额 '+result.balance+'（'+result.balanceCurrency+'），活跃订单 '+result.activeOrders+'。报价 '+result.quote.price+'（'+result.quote.currency+'），库存 '+result.quote.stock+'；这是查询快照，下单以回执为准。';
        else message='固定代理连通成功；请求地区 US，未核验实际出口地区。未更换出口。';
      }
      output.textContent=message+' 查询于 '+new Date(answer.checkedAt).toLocaleString();
    } catch(error) {output.dataset.ok='false';output.textContent=error.message;}
    finally {button.disabled=false;resourceChecking--;resourceForm.querySelector('button[type=submit]').disabled=resourceChecking>0;}
  });
  function automaticMode() {
    const automatic = field('automatic').checked;
    for (const name of ['email','password','phone','platform','activation','expiresAt']) {
      field(name).disabled=automatic; field(name).closest('label').hidden=automatic;
    }
    for (const option of field('phonecc').options) option.disabled=automatic && !['PL','CO','US','PT'].includes(option.value);
    if(automatic) field('phonecc').value=setup?.resources?.phoneCountry||'PL';
    field('phonecc').disabled=automatic;
    $('registerPasswordHint').hidden=automatic; $('registerExpiryHint').hidden=automatic;
    $('registerAutomaticHint').hidden=!automatic;
    const ready=setup && setup.purchaseEnabled && setup.budgetConfigured && setup.mailboxConfigured && setup.smsConfigured && (setup.resources.durationHours!==24 || setup.resources.acceptRentalPriceChange);
    $('registerAutomaticHint').textContent=ready ? '本机配置齐备；开始前将核实绑定邮箱订单，再按保存的 Hero 预算购号。创建与激活分别核验。' : '尚不能自动建号：请在注册资源中绑定邮箱订单、配置 Hero 与预算，并核对服务端购号开关。';
    const role=field('familyRole').value, mother=role==='organizer', member=role==='member';
    $('registerFamilyRegionLabel').hidden=!mother;field('familyRegion').disabled=!mother;
    $('registerOrganizerLabel').hidden=!member; field('organizer').disabled=!member; field('organizer').required=member;
    $('registerSubscriptionConsent').hidden=!mother; field('acceptSubscription').disabled=!mother;field('acceptSubscription').required=mother;
    const familyReady=mother ? (field('familyRegion').value==='CN'||setup?.subscriptionEnabled) : member ? !!field('organizer').value : true;
    $('registerFamilyHint').textContent=mother ? (field('familyRegion').value==='CN'||setup?.subscriptionEnabled ? '国区母号在场付款后继续关闭续费、建组和共享；美区保留原流程。' : '自动订阅开关关闭，母号任务暂不能启动。') : member ? '成员只加入指定母号的家庭并验证共享储存，不购买订阅。' : '完成激活后停止，不执行家庭或订阅操作。';
    $('registerSubmit').disabled=submitting || (automatic && !ready) || !familyReady;
  }
  field('automatic').addEventListener('change',automaticMode);
  field('familyRegion').addEventListener('change',automaticMode);
  field('familyRole').addEventListener('change',()=>{familySelected=true;automaticMode();});
  field('organizer').addEventListener('change',automaticMode);
  async function loadSetup() {
    setup=await api('/config');
    if(setup.resources?.proxyPort) field('proxyPort').value=setup.resources.proxyPort;
    const prior=field('organizer').value, mothers=setup.familyOrganizers||[];
    field('organizer').replaceChildren(...(mothers.length ? mothers.map(m=>new Option(m.name||m.id,m.id)) : [new Option('请先完成一个母号','')]));
    if(mothers.some(m=>m.id===prior)) field('organizer').value=prior;
    if(!familySelected) field('familyRole').value=mothers.length ? 'member':'organizer';
    automaticMode();
  }
  function notice(message) { $('registerNotice').textContent = message; $('registerNotice').hidden = !message; }
  async function api(path, body) {
    const response = await fetch('/api/register'+path, {method:body === undefined ? 'GET':'POST',headers:{'X-App-Token':token,'Content-Type':'application/json'},body:body === undefined ? undefined:JSON.stringify(body)});
    let value; try { value = await response.json(); } catch { throw Error('面板响应异常，请刷新后查看原任务'); }
    if (!response.ok) throw Error(value.message || '请求未完成，请查看原任务');
    return value;
  }
  function showForm(show) {
    form.hidden = !show; $('registerToggle').setAttribute('aria-expanded',String(show));
    $('registerToggle').textContent = show ? '收起表单':'新建账号';
  }
  $('registerToggle').addEventListener('click', () => {showForm(form.hidden);if(!form.hidden) loadSetup().catch(error=>notice(error.message));});
  function validatePassword() {
    if (field('automatic').checked) { field('password').setCustomValidity(''); return; }
    const value = field('password').value;
    field('password').setCustomValidity(value.length >= 8 && /[A-Z]/.test(value) && /[a-z]/.test(value) && /[0-9]/.test(value) && /[^A-Za-z0-9]/.test(value) ? '' : '密码需至少 8 位，包含大小写字母、数字和特殊字符');
  }
  field('password').addEventListener('input',validatePassword);
  function browserMode() {
    const local=field('browserMode').value==='local';
    for(const option of field('mode').options) option.disabled=(option.value==='roxy' && local)||(option.value==='local' && !local);
    if(field('mode').selectedOptions[0]?.disabled) field('mode').value=local?'local':'roxy';
    const fixed=field('mode').value==='roxy';
    $('registerPortLabel').hidden=!fixed;field('proxyPort').disabled=!fixed;
    $('registerBrowserHint').textContent=local?'本机 Chrome 独立打开窗口，图形校验需在场人工完成。方式与代理保存到当前任务。':'Forge 管理浏览器窗口与环境，沿用现有流程。方式与代理保存到当前任务。';
  }
  field('browserMode').addEventListener('change',()=>{browserMode();try{localStorage.setItem('registrationBrowserMode',field('browserMode').value);}catch{}});
  field('mode').addEventListener('change',browserMode);
  try {const saved=localStorage.getItem('registrationBrowserMode');if(['local','forge'].includes(saved))field('browserMode').value=saved;}catch{}
  browserMode();
  form.addEventListener('submit', async event => {
    event.preventDefault(); validatePassword(); if (!form.reportValidity() || submitting) return;
    const expiry = new Date(field('expiresAt').value);
    const automatic=field('automatic').checked;
    if (!automatic && (!Number.isFinite(expiry.getTime()) || expiry <= new Date())) { notice('请填写接码平台的实际到期时间，号码不能已过期。'); return; }
    submitting=true; $('registerSubmit').disabled=true; notice('正在创建任务…');
    try {
      const manual=automatic ? {} : {email:field('email').value.trim(),password:field('password').value,phone:field('phone').value,platform:field('platform').value,activation:field('activation').value,expiresAt:expiry.toISOString(),...(retryOf ? {retryOf}: {})};
      const role=field('familyRole').value;
      const family={role,...(role==='member'?{organizer:field('organizer').value}:{}),...(role==='organizer'?{region:field('familyRegion').value,acceptSubscription:field('acceptSubscription').checked}:{})};
      const result=await api('',{automatic,...manual,family,browserMode:field('browserMode').value,phonecc:field('phonecc').value,acceptTerms:field('acceptTerms').checked,captchaProvider:field('browserMode').value==='local'?'manual':'yes',egress:{mode:field('mode').value,proxyPort:Number(field('proxyPort').value)}});
      selected=result.jobId; retryOf=''; field('password').value=''; $('registerRetryNote').hidden=true;
      field('acceptSubscription').checked=false;familySelected=false;
      showForm(false); notice(automatic ? '任务已提交，邮箱和短信验证码将自动读取。' : '任务已提交。请留意邮箱验证码；短信由接码平台读取。');
      await refresh();
    } catch (error) { notice(error.message); }
    finally { submitting=false; automaticMode(); }
  });
  function render(job) {
    $('registerState').textContent=(job.browserMode==='local'?'本机 Chrome':'Forge')+' · '+(stageNames[job.stage] || '处理')+' · '+(stateNames[job.state] || '待核实');
    $('registerElapsed').textContent='用时 '+Math.max(0,Math.floor(job.elapsedMs/60000))+' 分 '+Math.max(0,Math.floor(job.elapsedMs/1000)%60)+' 秒';
    const index=job.stage==='done' ? stages.length:stages.indexOf(job.stage);
    for (const li of $('registerStages').children) {
      li.hidden=(li.dataset.stage==='subscription' && job.familyRole!=='organizer') || (li.dataset.stage==='family' && !['organizer','member'].includes(job.familyRole));
      const i=stages.indexOf(li.dataset.stage);
      li.classList.toggle('complete',i<index); li.classList.toggle('current',i===index);
      if(i===index) li.setAttribute('aria-current','step'); else li.removeAttribute('aria-current');
    }
    let detail='正在处理；普通等码超时会保留页面和号码，直到号码到期。';
    if (job.state==='ready') detail=job.handoffComplete ? '账号已加入列表，现有面板已完成激活并确认后台在线。' : '流程完成，请在账号列表核对当前状态。';
    else if (job.state==='rate_limited') detail='已停止发送验证码和登录。冷却至 '+(job.cooldownUntil ? new Date(job.cooldownUntil).toLocaleString():'稍后')+'，之后需核实状态再继续。';
    else if (['unknown','needs_review','needs_login'].includes(job.state)) detail='请先查看证据并核实账号状态。不会自动重复注册或登录。';
    else if (job.state==='cancelled') detail='任务已停止。可能已经提交的注册需要核实，不会重新提交。';
    else if (['failed','usage'].includes(job.state)) detail='流程未完成。请查看运行记录与证据；只有明确失败才允许用新号码重试。';
    if(job.refundPending) detail+=' 退号待结算，后台将按平台冷却重试。';
    if(job.refundDeferred) detail+=' 号码暂时保留，到期后结算。';
    if(job.refundConfirmed) detail+=' 平台已确认退号。';
    if(job.cleanupPending) detail+=' 浏览器清理待重试。';
    if(job.message) detail+=' '+job.message;
    if(job.familyComplete) detail+=job.familyRole==='organizer' ? ' 母号的 iCloud+ 和家庭储存共享均已确认。' : ' 已加入指定母号的家庭，共享储存已生效。';
    if(job.spend) detail+=' 购号 '+job.spend.purchases+' 次，累计 '+job.spend.gross+' '+job.spend.currency+'，已退 '+job.spend.refunded+'。';
    $('registerDetail').textContent=detail;
    $('registerCodeForm').hidden=!(job.active && job.stage==='creating' && !job.automatic);
    $('registerRetry').hidden=!job.canRetry;
    $('registerContinuePayment').hidden=job.active||job.stage!=='subscription'||job.state!=='waiting_input';
    $('registerCancel').hidden=['ready','cancelled'].includes(job.state);
  }
  $('registerContinuePayment').addEventListener('click',async()=>{try{await api('/'+selected+'/continue-payment',{});notice('正在核验付款结果并继续');await refresh();}catch(error){notice(error.message);}});
  async function refresh() {
    if(polling) return; polling=true;
    try {
      const result=await api(''); jobs=result.jobs.sort((a,b)=>new Date(b.startedAt)-new Date(a.startedAt));
      $('registerJobs').hidden=!jobs.length;
      if(!jobs.length) return;
      if(!jobs.some(job=>job.jobId===selected)) selected=jobs[0].jobId;
      const select=$('registerJobSelect');
      if(Array.from(select.options).map(o=>o.value).join()!==jobs.map(j=>j.jobId).join()) {
        select.replaceChildren(...jobs.map(job=>new Option(new Date(job.startedAt).toLocaleString()+' · '+job.jobId.slice(-6),job.jobId)));
      }
      select.value=selected; render(jobs.find(job=>job.jobId===selected));
      const id=selected, rows=await api('/'+id+'/log?tail=200');
      if(id===selected) $('registerLog').textContent=rows.map(row=>new Date(row.at).toLocaleTimeString()+'  '+(stageNames[row.stage]||'处理')+' · '+(stateNames[row.state]||'待核实')).join('\n') || '暂无记录';
    } catch (error) { notice(error.message); }
    finally { polling=false; }
  }
  $('registerJobSelect').addEventListener('change',()=> { selected=$('registerJobSelect').value; $('registerEvidenceList').replaceChildren(); $('registerEvidenceList').hidden=true; $('registerEmailCode').value=''; refresh(); });
  $('registerCodeForm').addEventListener('submit',async event=> {
    event.preventDefault(); const code=$('registerEmailCode'); const value=code.value; code.value='';
    try { await api('/'+selected+'/input',{code:value}); notice('邮箱验证码已交给当前任务。'); } catch(error) { notice(error.message); }
  });
  $('registerCancel').addEventListener('click',async()=> {
    if(!confirm('停止当前任务？已提交的注册可能已经成功，号码会按实际状态结算。')) return;
    try { const result=await api('/'+selected+'/cancel',{}); notice(result.message); await refresh(); } catch(error) { notice(error.message); }
  });
  $('registerRetry').addEventListener('click',()=> {
    if(!confirm('重试将创建新任务，需要重新购买号码。确认准备新的号码？')) return;
    retryOf=selected;
    field('browserMode').value=jobs.find(job=>job.jobId===selected)?.browserMode||'forge';browserMode();
    field('automatic').checked=false; automaticMode();
    for(const name of ['password','phone','activation','expiresAt']) field(name).value='';
    $('registerRetryNote').hidden=false; showForm(true); field('email').focus();
  });
  $('registerEvidence').addEventListener('click',async()=> {
    const id=selected, list=$('registerEvidenceList'); list.replaceChildren(); list.hidden=false;
    try {
      const names=await api('/'+id+'/evidence');
      if(!names.length) { list.textContent='当前任务尚无截图。'; return; }
      for(const name of names) {
        if(selected!==id) return;
        const figure=document.createElement('figure'), caption=document.createElement('figcaption'), button=document.createElement('button');
        caption.textContent=name; button.className='text-button'; button.textContent='打开截图'; button.type='button';
        button.addEventListener('click',async()=> {
          button.disabled=true;
          try {
            const response=await fetch('/api/register/'+id+'/evidence?name='+encodeURIComponent(name),{headers:{'X-App-Token':token}});
            if(!response.ok) throw Error('截图无法读取');
            const blob=await response.blob();
            const reader=new FileReader(); reader.onload=()=> { const img=new Image(); img.alt=name; img.src=reader.result; button.replaceWith(img); }; reader.readAsDataURL(blob);
          } catch(error) { button.disabled=false; notice(error.message); }
        });
        figure.append(button,caption); list.append(figure);
      }
    } catch(error) { notice(error.message); }
  });
  automaticMode(); loadSetup().catch(error=>notice(error.message)); loadResources().catch(error=>resourceNotice(error.message));
  refresh(); setInterval(refresh,3000);
})();
