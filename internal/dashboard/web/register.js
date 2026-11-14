(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const token = document.querySelector('meta[name="app-token"]').content;
  const form = $('registerForm');
  const field = name => form.elements.namedItem(name);
  const stages = ['creating','login','terms','twofa','device','keepalive'];
  const stageNames = {queued:'排队',creating:'注册',create:'注册',login:'网页登录',terms:'服务条款',twofa:'账户验证',gsa:'账户验证',device:'设备登记',keepalive:'会话验证',done:'完成'};
  const stateNames = {queued:'等待开始',running:'正在进行',waiting_input:'等待验证码',ready:'流程已完成',failed:'失败',usage:'参数需要修正',unknown:'结果待核实',needs_review:'需要核实',rate_limited:'Apple 要求冷却',needs_login:'会话需要重新验证',cancelled:'已取消'};
  let selected = '', retryOf = '', submitting = false, polling = false;
  let jobs = [];
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
  $('registerToggle').addEventListener('click', () => showForm(form.hidden));
  function validatePassword() {
    const value = field('password').value;
    field('password').setCustomValidity(value.length >= 8 && /[A-Z]/.test(value) && /[a-z]/.test(value) && /[0-9]/.test(value) && /[^A-Za-z0-9]/.test(value) ? '' : '密码需至少 8 位，包含大小写字母、数字和特殊字符');
  }
  field('password').addEventListener('input',validatePassword);
  field('mode').addEventListener('change',() => { const direct=field('mode').value==='direct'; $('registerPortLabel').hidden=direct; field('proxyPort').disabled=direct; });
  form.addEventListener('submit', async event => {
    event.preventDefault(); validatePassword(); if (!form.reportValidity() || submitting) return;
    const expiry = new Date(field('expiresAt').value);
    if (!Number.isFinite(expiry.getTime()) || expiry <= new Date()) { notice('请填写接码平台的实际到期时间，号码不能已过期。'); return; }
    submitting=true; $('registerSubmit').disabled=true; notice('正在创建任务…');
    try {
      const result=await api('',{email:field('email').value.trim(),password:field('password').value,phone:field('phone').value,phonecc:field('phonecc').value,platform:field('platform').value,activation:field('activation').value,expiresAt:expiry.toISOString(),acceptTerms:field('acceptTerms').checked,captchaProvider:'yes',egress:{mode:field('mode').value,proxyPort:Number(field('proxyPort').value)},...(retryOf ? {retryOf}: {})});
      selected=result.jobId; retryOf=''; field('password').value=''; $('registerRetryNote').hidden=true;
      showForm(false); notice('任务已提交。请留意邮箱验证码；短信由接码平台读取。');
      await refresh();
    } catch (error) { notice(error.message); }
    finally { submitting=false; $('registerSubmit').disabled=false; }
  });
  function render(job) {
    $('registerState').textContent=(stageNames[job.stage] || '处理')+' · '+(stateNames[job.state] || '待核实');
    $('registerElapsed').textContent='用时 '+Math.max(0,Math.floor(job.elapsedMs/60000))+' 分 '+Math.max(0,Math.floor(job.elapsedMs/1000)%60)+' 秒';
    const index=job.stage==='done' ? stages.length:stages.indexOf(job.stage);
    for (const li of $('registerStages').children) {
      const i=stages.indexOf(li.dataset.stage);
      li.classList.toggle('complete',i<index); li.classList.toggle('current',i===index);
      if(i===index) li.setAttribute('aria-current','step'); else li.removeAttribute('aria-current');
    }
    let detail='正在处理；普通等码超时会保留页面和号码，直到号码到期。';
    if (job.state==='ready') detail=job.imported ? '账号已加入列表。面板运行期间，每 10 分钟验证网页会话；设备受信任状态尚未证实。':'流程完成，正在核对账号会话并加入列表。';
    else if (job.state==='rate_limited') detail='已停止发送验证码和登录。冷却至 '+(job.cooldownUntil ? new Date(job.cooldownUntil).toLocaleString():'稍后')+'，之后需核实状态再继续。';
    else if (['unknown','needs_review','needs_login'].includes(job.state)) detail='请先查看证据并核实账号状态。不会自动重复注册或登录。';
    else if (job.state==='cancelled') detail='任务已停止。可能已经提交的注册需要核实，不会重新提交。';
    else if (['failed','usage'].includes(job.state)) detail='流程未完成。请查看运行记录与证据；只有明确失败才允许用新号码重试。';
    if(job.refundPending) detail+=' 退号待结算，后台将按平台冷却重试。';
    if(job.refundDeferred) detail+=' 号码暂时保留，到期后结算。';
    if(job.refundConfirmed) detail+=' 平台已确认退号。';
    if(job.cleanupPending) detail+=' 浏览器清理待重试。';
    if(job.message) detail+=' '+job.message;
    $('registerDetail').textContent=detail;
    $('registerCodeForm').hidden=!(job.active && job.stage==='creating');
    $('registerRetry').hidden=!job.canRetry;
    $('registerCancel').hidden=['ready','cancelled'].includes(job.state);
  }
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
  refresh(); setInterval(refresh,3000);
})();
