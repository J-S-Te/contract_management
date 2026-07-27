package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

func (h *Handler) webHome(c *gin.Context) {
	if _, err := h.identity.Authenticate(c.Request.Context(), c.Request); err != nil {
		if err == platform.ErrUnauthenticated {
			c.Redirect(http.StatusFound, "auth/login")
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
    header a{color:#172554;background:white;text-decoration:none}main{max-width:1240px;margin:28px auto;padding:0 20px}
    .toolbar{display:flex;gap:12px;padding:16px;border:1px solid #dbe3ef;border-radius:12px;background:white}
    input,select{min-height:40px;border:1px solid #cbd5e1;border-radius:8px;padding:0 12px;background:white}
    input{flex:1}.toolbar button{color:white;background:#2563eb}.card{margin-top:16px;overflow:auto;border:1px solid #dbe3ef;border-radius:12px;background:white}
    table{width:100%;border-collapse:collapse}th,td{padding:14px 16px;border-bottom:1px solid #e2e8f0;text-align:left;white-space:nowrap}
    th{color:#64748b;background:#f8fafc;font-size:12px}td strong,td small{display:block}td small{margin-top:4px;color:#64748b}
    .state{padding:80px 24px;text-align:center;color:#64748b}.status{padding:4px 9px;border-radius:999px;color:#1d4ed8;background:#dbeafe;font-size:12px}
    @media(max-width:700px){header{padding:18px}main{margin-top:18px}.toolbar{flex-direction:column}input{width:100%}}
  </style>
</head>
<body>
  <header><div><strong>合同管理系统</strong><p>统一身份已接入 · 合同全生命周期台账</p></div><a href="auth/logout">退出</a></header>
  <main>
    <section class="toolbar"><input id="keyword" type="search" placeholder="搜索合同编号或标题"><select id="status"><option value="">全部状态</option><option>DRAFT</option><option>APPROVING</option><option>APPROVED</option><option>ACTIVE</option><option>ARCHIVED</option></select><button id="refresh">刷新</button></section>
    <section class="card"><div id="content" class="state">正在读取合同台账…</div></section>
  </main>
  <script>
    const content=document.querySelector('#content'), keyword=document.querySelector('#keyword'), status=document.querySelector('#status');
    let items=[];
    const esc=value=>String(value??'').replace(/[&<>"']/g,char=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
    const money=item=>new Intl.NumberFormat('zh-CN',{style:'currency',currency:item.currency||'CNY'}).format((item.amount_minor||0)/100);
    function render(){const query=keyword.value.trim().toLowerCase(),state=status.value;const rows=items.filter(item=>(!state||item.status===state)&&(!query||[item.contract_number,item.title].join(' ').toLowerCase().includes(query)));
      if(!rows.length){content.className='state';content.textContent='当前没有符合条件的合同。';return}
      content.className='';content.innerHTML='<table><thead><tr><th>合同编号 / 标题</th><th>类型</th><th>金额</th><th>状态</th><th>更新时间</th></tr></thead><tbody>'+rows.map(item=>'<tr><td><strong>'+esc(item.title)+'</strong><small>'+esc(item.contract_number)+'</small></td><td>'+esc(item.contract_type||'—')+'</td><td>'+esc(money(item))+'</td><td><span class="status">'+esc(item.status)+'</span></td><td>'+esc(item.updated_at?new Date(item.updated_at).toLocaleDateString('zh-CN'):'—')+'</td></tr>').join('')+'</tbody></table>'}
    async function load(){content.className='state';content.textContent='正在读取合同台账…';try{const response=await fetch('api/v1/contracts?limit=100',{credentials:'include',headers:{Accept:'application/json'}});if(response.status===401){location.href='auth/login';return}const body=await response.json();if(!response.ok)throw new Error(body.message||'读取失败');items=Array.isArray(body.data)?body.data:[];render()}catch(error){content.className='state';content.textContent='读取合同台账失败：'+error.message}}
    keyword.addEventListener('input',render);status.addEventListener('change',render);document.querySelector('#refresh').addEventListener('click',load);load();
  </script>
</body></html>`
