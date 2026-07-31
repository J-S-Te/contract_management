package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

func (h *Handler) webHome(c *gin.Context) {
	if _, err := h.identity.Authenticate(c.Request.Context(), c.Request); err != nil {
		if err == platform.ErrUnauthenticated {
			loginPath := "/auth/login"
			if resolver, ok := h.identity.(PublicPathResolver); ok {
				loginPath = resolver.PublicPath(loginPath)
			}
			c.Redirect(http.StatusFound, loginPath)
			return
		}
		c.String(http.StatusServiceUnavailable, "身份服务暂不可用")
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, contractApplicationHTML)
}

func (h *Handler) loggedOut(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, loggedOutHTML)
}

const loggedOutHTML = `<!doctype html>
<html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>已退出 · 合同管理系统</title>
<style>body{display:grid;place-items:center;min-height:100vh;margin:0;font:16px system-ui;background:#f4f7fb;color:#172033}main{padding:40px;border:1px solid #dbe3ef;border-radius:16px;background:white;text-align:center;box-shadow:0 12px 32px #0f172a12}a{color:#2563eb}</style>
<main><h1>已安全退出</h1><p>合同系统本地会话已清除。</p><a href="./">重新进入合同管理系统</a></main></html>`

const contractApplicationHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>合同管理系统</title>
  <style>
    *{box-sizing:border-box}body{margin:0;font:14px system-ui,-apple-system,sans-serif;color:#172033;background:#f4f7fb}
    header{display:flex;align-items:center;justify-content:space-between;padding:22px 32px;color:white;background:#172554}
    header p{margin:4px 0 0;color:#bfdbfe}button,a{border:0;border-radius:8px;padding:9px 14px;cursor:pointer}
    header a{color:#172554;background:white;text-decoration:none}.header-actions{display:flex;gap:8px}.header-actions button{color:#172554;background:#dbeafe}main{max-width:1240px;margin:28px auto;padding:0 20px}
    .toolbar{display:flex;gap:12px;padding:16px;border:1px solid #dbe3ef;border-radius:12px;background:white}
    input,select,textarea{min-height:40px;border:1px solid #cbd5e1;border-radius:8px;padding:8px 12px;background:white;font:inherit}
    input{flex:1}.toolbar button{color:white;background:#2563eb}.card{margin-top:16px;overflow:auto;border:1px solid #dbe3ef;border-radius:12px;background:white}
    table{width:100%;border-collapse:collapse}th,td{padding:14px 16px;border-bottom:1px solid #e2e8f0;text-align:left;white-space:nowrap}
    th{color:#64748b;background:#f8fafc;font-size:12px}td strong,td small{display:block}td small{margin-top:4px;color:#64748b}
    .state{padding:80px 24px;text-align:center;color:#64748b}.status{padding:4px 9px;border-radius:999px;color:#1d4ed8;background:#dbeafe;font-size:12px}
    .link{padding:0;color:#2563eb;background:none}.hidden{display:none!important}dialog{width:min(980px,calc(100% - 32px));max-height:90vh;padding:0;border:0;border-radius:16px;box-shadow:0 24px 80px #0f172a55}dialog::backdrop{background:#0f172a88}
    .modal-head{display:flex;align-items:center;justify-content:space-between;padding:18px 22px;border-bottom:1px solid #e2e8f0}.modal-head h2{margin:0}.close{font-size:22px;background:none}.modal-body{padding:22px;overflow:auto;max-height:calc(90vh - 70px)}
    .form-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px}.field{display:grid;gap:6px}.field label{font-weight:600}.field input,.field select,.field textarea{width:100%}.full{grid-column:1/-1}
    .template-fields{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px;padding:16px;border-radius:10px;background:#f8fafc}.actions{display:flex;justify-content:flex-end;gap:10px;margin-top:18px}.primary{color:white;background:#2563eb}.secondary{color:#1e40af;background:#dbeafe}
    .preview{min-height:260px;max-height:480px;overflow:auto;padding:34px 42px;border:1px solid #cbd5e1;background:white;box-shadow:inset 0 0 0 8px #f8fafc}.docx-preview p{margin:.65em 0;line-height:1.75}.notice{margin:0 0 14px;padding:10px 12px;border-radius:8px;color:#1e40af;background:#eff6ff}
    @media(max-width:700px){header{padding:18px;align-items:flex-start}.header-actions{flex-wrap:wrap;justify-content:flex-end}main{margin-top:18px}.toolbar{flex-direction:column}input{width:100%}.form-grid,.template-fields{grid-template-columns:1fr}.full{grid-column:auto}}
  </style>
</head>
<body>
  <header><div><strong>合同管理系统</strong><p>统一身份已接入 · 合同全生命周期台账</p></div><div class="header-actions"><button id="upload-template" class="hidden">上传模板</button><button id="new-contract" class="hidden">新建合同</button><a href="auth/logout">退出</a></div></header>
  <main>
    <section class="toolbar"><input id="keyword" type="search" placeholder="搜索合同编号或标题"><select id="status"><option value="">全部状态</option><option>DRAFT</option><option>APPROVING</option><option>APPROVED</option><option>ACTIVE</option><option>ARCHIVED</option></select><button id="refresh">刷新</button></section>
    <section class="card"><div id="content" class="state">正在读取合同台账…</div></section>
  </main>
  <dialog id="contract-dialog"><div class="modal-head"><h2>新建合同</h2><button class="close" data-close="contract-dialog">×</button></div><div class="modal-body">
    <p class="notice">选择 DOCX 模板并填写变量，可先预览版式，再保存合同并导出原始 DOCX。</p>
    <form id="contract-form" class="form-grid">
      <div class="field"><label>合同编号</label><input name="contract_number" required></div>
      <div class="field"><label>合同标题</label><input name="title" required></div>
      <div class="field"><label>合同类型</label><input name="contract_type" required placeholder="例如：服务合同"></div>
      <div class="field"><label>服务类型</label><input name="service_type" required placeholder="例如：咨询服务"></div>
      <div class="field"><label>合同金额（元）</label><input name="amount" type="number" min="0" step="0.01" required></div>
      <div class="field"><label>币种</label><select name="currency"><option>CNY</option><option>USD</option><option>EUR</option></select></div>
      <div class="field full"><label>合同模板</label><select id="template-select" name="template_id" required><option value="">请选择模板</option></select></div>
      <div id="template-fields" class="template-fields full"><span>选择模板后显示需要填写的合同字段。</span></div>
      <div id="preview-wrap" class="field full hidden"><label>合同预览</label><div id="preview" class="preview"></div></div>
      <div class="actions full"><button type="button" class="secondary" id="preview-button">预览</button><button type="submit" class="primary">保存合同</button></div>
    </form>
  </div></dialog>
  <dialog id="upload-dialog"><div class="modal-head"><h2>上传合同模板</h2><button class="close" data-close="upload-dialog">×</button></div><div class="modal-body">
    <p class="notice">仅支持不超过 10MB 的 DOCX。请在 Word 中使用 <code>{{customer_name:客户名称}}</code> 形式标记待填写字段，冒号后的中文标签可省略。</p>
    <form id="upload-form" class="form-grid"><div class="field full"><label>模板名称</label><input name="name" required></div><div class="field full"><label>DOCX 文件</label><input name="file" type="file" accept=".docx,application/vnd.openxmlformats-officedocument.wordprocessingml.document" required></div><div class="actions full"><button class="primary" type="submit">上传模板</button></div></form>
  </div></dialog>
  <script>
    const content=document.querySelector('#content'), keyword=document.querySelector('#keyword'), status=document.querySelector('#status'), contractDialog=document.querySelector('#contract-dialog'), uploadDialog=document.querySelector('#upload-dialog');
    let items=[],templates=[],currentTemplate=null,currentUser=null;
    const esc=value=>String(value??'').replace(/[&<>"']/g,char=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
    const money=item=>new Intl.NumberFormat('zh-CN',{style:'currency',currency:item.currency||'CNY'}).format((item.amount_minor||0)/100);
    function render(){const query=keyword.value.trim().toLowerCase(),state=status.value;const rows=items.filter(item=>(!state||item.status===state)&&(!query||[item.contract_number,item.title].join(' ').toLowerCase().includes(query)));
      if(!rows.length){content.className='state';content.textContent='当前没有符合条件的合同。';return}
      content.className='';content.innerHTML='<table><thead><tr><th>合同编号 / 标题</th><th>类型</th><th>金额</th><th>状态</th><th>更新时间</th><th>文档</th></tr></thead><tbody>'+rows.map(item=>'<tr><td><strong>'+esc(item.title)+'</strong><small>'+esc(item.contract_number)+'</small></td><td>'+esc(item.contract_type||'—')+'</td><td>'+esc(money(item))+'</td><td><span class="status">'+esc(item.status)+'</span></td><td>'+esc(item.updated_at?new Date(item.updated_at).toLocaleDateString('zh-CN'):'—')+'</td><td>'+(item.template_id?'<a class="link" href="api/v1/contracts/'+encodeURIComponent(item.id)+'/export">导出 DOCX</a>':'—')+'</td></tr>').join('')+'</tbody></table>'}
    async function load(){content.className='state';content.textContent='正在读取合同台账…';try{const response=await fetch('api/v1/contracts?limit=100',{credentials:'include',headers:{Accept:'application/json'}});if(response.status===401){location.href='auth/login';return}const body=await response.json();if(!response.ok)throw new Error(body.message||'读取失败');items=Array.isArray(body.data)?body.data:[];render()}catch(error){content.className='state';content.textContent='读取合同台账失败：'+error.message}}
    async function request(url,options){const response=await fetch(url,Object.assign({credentials:'include',headers:{Accept:'application/json'}},options||{}));const body=await response.json();if(!response.ok)throw new Error(body.message||'操作失败');return body.data}
    async function initialize(){try{currentUser=await request('api/v1/auth/me');const permissions=currentUser.permissions||[],isAdmin=(currentUser.roles||[]).includes('admin');if(permissions.includes('contract.create')){document.querySelector('#new-contract').classList.remove('hidden');await loadTemplates()}if(isAdmin)document.querySelector('#upload-template').classList.remove('hidden')}catch(error){console.error(error)}}
    async function loadTemplates(){templates=await request('api/v1/contract-templates');const select=document.querySelector('#template-select');select.innerHTML='<option value="">请选择模板</option>'+templates.map(item=>'<option value="'+esc(item.id)+'">'+esc(item.name)+'</option>').join('')}
    function templateValues(){const values={};document.querySelectorAll('#template-fields [data-template-field]').forEach(input=>values[input.dataset.templateField]=input.value);return values}
    document.querySelector('#template-select').addEventListener('change',event=>{currentTemplate=templates.find(item=>item.id===event.target.value)||null;const holder=document.querySelector('#template-fields');holder.innerHTML=currentTemplate?currentTemplate.fields.map(field=>'<div class="field"><label>'+esc(field.label)+'</label><input data-template-field="'+esc(field.name)+'" required></div>').join(''):'<span>选择模板后显示需要填写的合同字段。</span>';document.querySelector('#preview-wrap').classList.add('hidden')});
    document.querySelector('#preview-button').addEventListener('click',async()=>{if(!document.querySelector('#contract-form').reportValidity())return;if(!currentTemplate){alert('请先选择合同模板');return}try{const data=await request('api/v1/contract-templates/'+encodeURIComponent(currentTemplate.id)+'/preview',{method:'POST',headers:{'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify({values:templateValues()})});document.querySelector('#preview').innerHTML=data.html;document.querySelector('#preview-wrap').classList.remove('hidden')}catch(error){alert(error.message)}});
    document.querySelector('#contract-form').addEventListener('submit',async event=>{event.preventDefault();if(!currentTemplate)return alert('请选择合同模板');const form=new FormData(event.target),payload={contract_number:form.get('contract_number'),title:form.get('title'),contract_type:form.get('contract_type'),service_type:form.get('service_type'),amount_minor:Math.round(Number(form.get('amount'))*100),currency:form.get('currency'),content:'',template_id:currentTemplate.id,template_values:templateValues()};try{const created=await request('api/v1/contracts',{method:'POST',headers:{'Content-Type':'application/json',Accept:'application/json'},body:JSON.stringify(payload)});contractDialog.close();event.target.reset();currentTemplate=null;await load();if(confirm('合同已保存，是否立即导出 DOCX？'))location.href='api/v1/contracts/'+encodeURIComponent(created.id)+'/export'}catch(error){alert(error.message)}});
    document.querySelector('#upload-form').addEventListener('submit',async event=>{event.preventDefault();try{await request('api/v1/contract-templates',{method:'POST',body:new FormData(event.target)});uploadDialog.close();event.target.reset();await loadTemplates();alert('模板上传成功')}catch(error){alert(error.message)}});
    document.querySelector('#new-contract').addEventListener('click',()=>contractDialog.showModal());document.querySelector('#upload-template').addEventListener('click',()=>uploadDialog.showModal());document.querySelectorAll('[data-close]').forEach(button=>button.addEventListener('click',()=>document.querySelector('#'+button.dataset.close).close()));
    keyword.addEventListener('input',render);status.addEventListener('change',render);document.querySelector('#refresh').addEventListener('click',load);load();
    initialize();
  </script>
</body></html>`
