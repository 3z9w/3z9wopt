/* ══════════════════════════════════════════════════════════════════════════
   服务大厅 · 独立模块（design/svc.js）· 2026-10-13 按 v2 老版本原样移植
   ---------------------------------------------------------------------------
   版面与内容 100% 照 v2（frontend/dist/app.js 第 2832~3146 行）：
     hero（头像 + 署名 + 身份 + 在线 + 大标题 + 两个动作 + 数据条 + 承诺条）
     → 服务项卡片墙（序号 / 重音色条 / 图标 / 名称 / 标签 / 说明 / 要点 / 时长 / 下单）
     → 服务流程（接入步骤 + 三种结局一行说清）
     → 真实口碑（评价 / 案例）
     → 联系卡（主按钮 + 诊断报告 + 二维码）
     + 两个弹窗：微信二维码（点了就出码）、机器诊断报告（打开即采集并复制）

   只换了皮：v2 是米白纸感 + 赭橙主色，这里是 v1.1 的深蓝玻璃 + 蓝青强调色，
   卡片圆角 / 描边 / 阴影全走 v1.1 变量，跟本机配置页同一套语言。

   数据全部来自后端 service.go：GetSvcPage（hero / stats / promise / items /
   flow / reviews）+ SvcLinks（qr / reward / wxid / site…）。页面上不写死任何
   服务项目、价格、承诺文案 —— 改服务内容改后端就行。

   ⚠️ 对外入口全部挂 window（主文件是同步 XHR + eval 的 IIFE 加载的）：
      renderSvc / bindSvc / loadSvc / svcDiagRun / SVC_LINKS / SV
      —— 命令行（cmdItems）与关于页都读 window.SVC_LINKS，别用裸变量。
   ⚠️ 数据到达时若页面停在 loading，必须整页 render（loading 页里没有 #svcBody，
      只认容器会永远转圈 —— 工具库那次的坑）。
   ══════════════════════════════════════════════════════════════════════════ */

var SV = {state:'loading', err:'', page:null, links:{}, loading:false, seq:0, report:''};
window.SV = SV;
window.SVC_LINKS = {};

/* v2 用的图标键在 v1.1 图标表里有一部分没有，这里做等价替换（图标语义不变） */
var SV_ICO = {globe:'box', wrench:'gear', download:'disk', help:'info', user:'monitor', star:'sparkle', phone:'message'};
function svIco(k, s){ return ico(SV_ICO[k] || k || 'box', s || 16); }

function svEsc(s){
  return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
    return {'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;'}[c];
  });
}
function svEl(id){ return document.getElementById(id); }
/* 页面标记打在 page-head 上：loading 态也有它，所以任何状态下都能认出"我在服务大厅页" */
function svOnPage(){ return !!document.querySelector('[data-svcpage]'); }

function svAfter(){
  if(svOnPage() && typeof render === 'function') render();
}
function svPaint(){
  var b = svEl('svcBody');
  if(b) b.innerHTML = svBodyHtml();
  else svAfter();
}

function loadSvc(){
  if(SV.loading) return;
  if(!hasGo()){
    if(inWails()){ SV.state = 'failed'; SV.err = '后端连接还没就绪（window.go 为空）'; }
    /* ⚠️ 门闩：svAfter() 里的 render() 会回到 loadSvc，不置 loading 就无限递归爆栈 */
    SV.loading = true;
    svAfter();
    SV.loading = false;
    return;
  }
  SV.loading = true; SV.state = 'loading';
  var seq = ++SV.seq;
  Promise.all([callGo('GetSvcPage'), callGo('SvcLinks')]).then(function(r){
    SV.loading = false;
    if(seq !== SV.seq) return;
    SV.page = r[0] || {}; SV.links = r[1] || {};
    window.SVC_LINKS = SV.links;
    SV.state = 'ready';
    svAfter();
  }).catch(function(e){
    SV.loading = false; SV.state = 'failed'; SV.err = String(e);
    svAfter();
  });
}
window.loadSvc = loadSvc;

/* ── 渲染 ─────────────────────────────────────────────────────────────────── */
function renderSvc(){
  var h = '<div class="page-head" data-svcpage="1"><div class="page-title">服务大厅</div>'
    + '<div class="page-sub">软件能自己解决的就自己解决；修不好的，微信找大叔远程看 —— 先诊断、再报价。</div></div>';

  if(SV.state === 'failed'){
    return h + '<div class="card"><div class="empty" style="padding:48px 20px">'
      + '<div class="empty-ico" style="color:var(--danger)">' + ico('warn', 28) + '</div>'
      + '<div class="empty-t">没能读到服务信息</div><div class="empty-d">' + svEsc(SV.err) + '</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      + '<button class="btn primary" data-svcact="retry">' + ico('refresh', 16) + '重新读取</button>'
      + '</div></div></div>';
  }
  if(SV.state !== 'ready'){
    return h + '<div class="card"><div class="empty" style="padding:56px 20px">'
      + '<div class="empty-ico" style="color:var(--info)">' + ico('refresh', 28) + '</div>'
      + '<div class="empty-t">正在读服务信息…</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      + '<button class="btn" data-svcact="retry">' + ico('refresh', 15) + '重新读取</button>'
      + '</div></div></div>';
  }
  return h + '<div id="svcBody">' + svBodyHtml() + '</div>';
}
window.renderSvc = renderSvc;

function svBodyHtml(){
  var p = SV.page || {};
  return '<div class="card svc-hero">' + svHeroHtml(p.hero || {}, p.stats || [], p.promise || []) + '</div>'
    + '<div class="svc-grid">' + (p.items || []).map(svCardHtml).join('') + '</div>'
    + '<div class="card svc-flow">' + svFlowHtml(p.flow || {}) + '</div>'
    + '<div class="card svc-social">'
    +   '<div class="svc-flow-t svc-soc-t"><span class="svc-soc-ic">' + svIco('heart', 15) + '</span>真实口碑'
    +     '<span class="svc-soc-verify">' + svIco('shield', 13) + '官网可查证</span></div>'
    +   '<div class="svc-flow-d">真实客户反馈，与官网 3z9w.com 公开内容一致</div>'
    +   '<div class="svc-reviews">' + (p.reviews || []).map(svReviewHtml).join('') + '</div>'
    + '</div>'
    + '<div class="card svc-contact">' + svContactHtml() + '</div>';
}

/* ── hero：左边"谁在服务"，右边"怎么开始"，下面数据条 + 承诺条（照 v2） ────── */
function svHeroHtml(h, stats, promise){
  var nm = h.name || '叔';
  var letter = svEsc(String(nm).slice(-1));
  var av = h.avatar ? '<img class="svc-av-img" src="' + svEsc(h.avatar) + '" alt="头像" onerror="this.remove()">' : '';
  return '<div class="svc-hero-top">'
    + '<div class="svc-who">'
    +   '<div class="svc-av-row">'
    +     '<div class="svc-avatar"><span class="svc-av-letter">' + letter + '</span>' + av
    +       '<span class="svc-av-badge">' + svIco('check', 11) + '</span></div>'
    +     '<div class="svc-who-t"><div class="svc-who-n">' + svEsc(h.name || '') + '</div>'
    +       '<div class="svc-who-chip">' + svIco('shield', 12) + '本人接单 · 不转包</div></div>'
    +   '</div>'
    +   '<div class="svc-who-i">' + svEsc(h.identity || '') + '</div>'
    +   '<div class="svc-who-on"><span class="svc-dot"></span>在线接单 · 微信直接沟通</div>'
    + '</div>'
    + '<div class="svc-hero-main">'
    +   '<div class="svc-h1">' + svEsc(h.title || '') + '</div>'
    +   '<div class="svc-hsub">' + svEsc(h.sub || '') + '</div>'
    +   '<div class="svc-hero-acts">'
    +     '<button class="btn primary" data-svcact="hire">' + svIco('message', 15) + svEsc(h.primary || '微信联系大叔') + '</button>'
    +     '<button class="btn" data-svcact="report">' + svIco('copy', 15) + svEsc(h.second || '生成诊断报告') + '</button>'
    +   '</div>'
    + '</div>'
    + '</div>'
    + '<div class="svc-hero-stats">' + (stats || []).map(svStatHtml).join('') + '</div>'
    + '<div class="svc-hero-promise">' + (promise || []).map(function(x){
        return '<span>' + svIco(x.icon || 'check', 14) + svEsc(x.text || '') + '</span>';
      }).join('') + '</div>';
}
function svStatHtml(s){
  return '<div class="svc-stat">' + svIco(s.icon || 'check', 15)
    + '<b>' + svEsc(s.value || '') + '</b><span>' + svEsc(s.label || '') + '</span></div>';
}

/* ── 服务项卡片：序号 + 重音色条 + 图标 + 名称 + 标签 + 说明 + 要点 + 时长 ──── */
function svCardHtml(it, i){
  var tagCls = it.tag === '紧急' ? 't-hot' : (it.tag === '热门' ? 't-pop' : 't-free');
  var num = String((i || 0) + 1);
  if(num.length < 2) num = '0' + num;
  return '<div class="svc-card" data-accent="' + ((i || 0) % 6) + '">'
    + '<i class="svc-num">' + num + '</i><i class="svc-bar"></i>'
    + '<div class="svc-top">'
    +   '<span class="svc-ic">' + svIco(it.icon || 'wrench', 17) + '</span>'
    +   '<div class="svc-name" title="' + svEsc(it.name) + '">' + svEsc(it.name) + '</div>'
    +   (it.tag ? '<i class="svc-tag ' + tagCls + '">' + svEsc(it.tag) + '</i>' : '')
    + '</div>'
    + '<div class="svc-desc">' + svEsc(it.desc) + '</div>'
    + '<ul class="svc-points">' + (it.points || []).map(function(x){ return '<li>' + svEsc(x) + '</li>'; }).join('') + '</ul>'
    + '<div class="svc-foot">'
    +   '<div class="svc-meta">' + svIco('clock', 13) + '预计 ' + svEsc(it.time) + '</div>'
    +   '<div class="svc-acts"><button class="btn primary sm" data-svcact="hire">'
    +     svIco('message', 13) + '1V1远程支持（付费）</button></div>'
    + '</div></div>';
}

/* ── 流程：上面一行接入步骤，下面一行把三种结局说清（照 v2 简化版） ────────── */
function svFlowHtml(f){
  if(!f || !f.title) return '';
  var st = f.steps || [];
  var steps = st.map(function(s, i){ return svStepHtml(s, i, st.length); }).join('');
  var ends = (f.ends || []).map(function(e){
    var tone = ['ok', 'warn', 'mute'].indexOf(e.tone) >= 0 ? e.tone : 'mute';
    return '<span class="fp-end t-' + tone + '"><i>' + svIco(e.icon || 'info', 13) + '</i>' + svEsc(e.text || '') + '</span>';
  }).join('');
  return '<div class="flow-head">'
    + '<div class="flow-t">' + svIco('bolt', 16) + svEsc(f.title || '') + '</div>'
    + '<div class="flow-d">' + svEsc(f.desc || '') + '</div></div>'
    + (steps ? '<div class="fp-steps" style="grid-template-columns:repeat(' + st.length + ',minmax(0,1fr))">' + steps + '</div>' : '')
    + (ends ? '<div class="fp-ends">' + ends + '</div>' : '');
}
function svStepHtml(s, i, n){
  var t = svEsc(s.title || ''), lt = String((s.linkText || '')).trim();
  var title = t;
  if(s.link && lt){
    var raw = String(s.title || '');
    var k = raw.indexOf(lt);
    if(k >= 0){
      title = svEsc(raw.slice(0, k)) + '<a class="svc-lk" data-url="' + svEsc(s.link) + '">' + svEsc(lt) + '</a>' + svEsc(raw.slice(k + lt.length));
    }
  }
  return '<div class="fp-step' + (i === n - 1 ? ' last' : '') + '">'
    + '<div class="fp-top"><span class="fp-no">' + (i + 1) + '</span>'
    + '<i class="fp-ic">' + svIco(s.icon || 'check', 15) + '</i></div>'
    + '<b>' + title + '</b><em>' + svEsc(s.desc || '') + '</em></div>';
}

/* ── 口碑：评价（星级）与案例（战绩）两种 ─────────────────────────────────── */
function svReviewHtml(r){
  var tags = (r.tags || []).map(function(t){ return '<em>' + svEsc(t) + '</em>'; }).join('');
  var hl = function(t){
    return svEsc(t).replace(/(\d[\d.]*\s*(?:帧|%|年)|\d[\d.]*\+)/g, '<b>$1</b>');
  };
  if(r.kind === 'case'){
    return '<div class="svc-rv"><div class="svc-rv-head">'
      + '<span class="svc-rv-av">' + svIco('chart', 16) + '</span>'
      + '<div><div class="svc-rv-n">服务案例</div><div class="svc-rv-m">' + svEsc(r.title || '') + '</div></div>'
      + '<span class="svc-rv-badge">' + svEsc(r.badge || '实战') + '</span></div>'
      + '<div class="svc-rv-x">' + hl(r.text || '') + '</div>'
      + '<div class="svc-rv-tags">' + tags + '</div></div>';
  }
  var n = Math.max(0, Math.min(5, r.stars || 0));
  return '<div class="svc-rv"><div class="svc-rv-head">'
    + '<span class="svc-rv-av">' + svEsc(String(r.name || '').slice(0, 1)) + '</span>'
    + '<div><div class="svc-rv-n">' + svEsc(r.name || '') + '</div><div class="svc-rv-m">' + svEsc(r.meta || '') + '</div></div>'
    + '<span class="svc-rv-star">' + '★★★★★'.slice(0, n) + '</span></div>'
    + '<div class="svc-rv-x">' + hl(r.text || '') + '</div>'
    + '<div class="svc-rv-tags">' + tags + '</div></div>';
}

/* ── 联系卡 ──────────────────────────────────────────────────────────────── */
function svContactHtml(){
  var qr = SV.links.qr || '', rw = SV.links.reward || '';
  var qrs = '';
  if(qr) qrs += '<div class="svc-qr"><img src="' + svEsc(qr) + '" alt="微信二维码" onerror="this.parentNode.style.display=\'none\'"><span>微信扫码添加</span></div>';
  if(rw) qrs += '<div class="svc-qr"><img src="' + svEsc(rw) + '" alt="赞赏码" onerror="this.parentNode.style.display=\'none\'"><span>请大叔喝杯奶茶</span></div>';
  return '<div class="svc-ct-main">'
    + '<div class="svc-ct-t">1V1 远程支持</div>'
    + '<div class="svc-ct-d">透明报价 · 平均 3 分钟响应 · 7×24 小时（凌晨节假日不休）</div>'
    + '<div class="svc-ct-acts">'
    +   '<button class="btn primary" data-svcact="hire">' + svIco('message', 15) + '微信联系大叔</button>'
    +   '<button class="btn" data-svcact="report">' + svIco('copy', 15) + '生成诊断报告</button>'
    + '</div>'
    + '<div class="svc-ct-tip">' + svIco('info', 14)
    +   '微信扫码添加大叔 —— 先把诊断报告发过去，沟通和报价都快</div>'
    + '</div>'
    + (qrs ? '<div class="svc-qrs">' + qrs + '</div>' : '');
}

/* ── 弹窗：v1.1 只有 #modal + #modalBox + .open（v2 的 modal() 在这里没有） ── */
function svModal(inner){
  var box = svEl('modalBox'), mod = svEl('modal');
  if(!box || !mod || typeof closeModal !== 'function'){ toast('弹窗容器没找到', 'warn'); return false; }
  box.innerHTML = inner;
  mod.classList.add('open');
  return true;
}

/* 微信二维码：全软件所有付费按钮都走这里 —— 点了就出码，不往外跳报修页 */
var SV_HIRING = false;
function svcHire(){
  if(SV_HIRING) return;
  SV_HIRING = true;
  var wxid = 'RESBJD', name = '无名大叔';
  if(SV.page && SV.page.hero && SV.page.hero.name) name = SV.page.hero.name;
  if(SV.links && SV.links.wxid) wxid = SV.links.wxid;
  var qr = (SV.links || {}).qr || '';
  if(!qr && hasGo()){
    callGo('SvcLinks').then(function(l){
      SV_HIRING = false;
      if(l){ SV.links = l; window.SVC_LINKS = l; if(l.wxid) wxid = l.wxid; }
      svHireModal(name, wxid, (SV.links || {}).qr || '');
    }).catch(function(){ SV_HIRING = false; svHireModal(name, wxid, ''); });
    return;
  }
  SV_HIRING = false;
  svHireModal(name, wxid, qr);
}
function svHireModal(name, wxid, qr){
  var h = '<div class="wxm-wrap"><button class="wxm-x" id="svWxmX">&times;</button>'
    + '<div class="wxm-app">' + svIco('message', 30) + '</div>'
    + '<div class="wxm-title">扫码添加 <em>' + svEsc(name) + '</em></div>'
    + '<div class="wxm-sub">描述故障 · 远程检查 · 再行报价</div>'
    + (qr ? '<div class="wxm-qr"><img src="' + svEsc(qr) + '" alt="微信二维码" onerror="this.style.display=\'none\'"></div>'
          : '<div class="wxm-qr"><div class="wxm-qr-ph">微信号 ' + svEsc(wxid) + '</div></div>')
    + '<div class="wxm-idrow"><b class="wxm-id">' + svEsc(wxid) + '</b>'
    +   '<button class="btn sm" id="svWxmCopy">' + svIco('copy', 13) + '复制</button></div>'
    + '<div class="wxm-note">' + svIco('info', 13) + '长按识别二维码 · 或复制微信号添加</div></div>';
  if(!svModal(h)) return;
  var x = svEl('svWxmX'); if(x) x.onclick = closeModal;
  var c = svEl('svWxmCopy');
  if(c) c.onclick = function(){ svCopy(wxid, '微信号已复制，去微信添加好友吧'); };
}

/* 机器诊断报告：打开即采集并写进剪贴板（跟 v2 手感一致） */
function svcDiagRun(){ svcGenReport(); }
window.svcDiagRun = svcDiagRun;

var SV_REPBUSY = false;
function svcGenReport(){
  var h = '<div class="modal-t">' + svIco('copy', 18) + '<span>机器诊断报告</span></div>'
    + '<div class="modal-d">一键汇总本机真实信息（系统 / 硬件 / 磁盘 / 蓝屏线索），微信直接发给大叔 —— 不用来回问配置，报价更快、判断更准</div>'
    + '<textarea class="svc-note" id="svRepNote" placeholder="（可选）一句话说下症状，例如：开机蓝屏 0x000000D1 进不去系统"></textarea>'
    + '<pre class="svc-out" id="svRepOut">正在采集本机信息…</pre>'
    + '<div class="modal-f"><button class="btn" id="svRepClose">关闭</button>'
    + '<span class="svc-f-gap"></span>'
    + '<button class="btn" id="svRepSend" style="display:none" data-svcact="hire">' + svIco('message', 14) + '扫码发给大叔</button>'
    + '<button class="btn primary" id="svRepCopy">' + svIco('copy', 14) + '复制报告</button></div>';
  if(!svModal(h)) return;
  var cl = svEl('svRepClose'); if(cl) cl.onclick = closeModal;
  var sd = svEl('svRepSend'); if(sd) sd.onclick = function(){ svcHire(); };
  var cp = svEl('svRepCopy'); if(cp) cp.onclick = function(){ svRepRun(true); };
  svRepRun(true);
}
function svRepRun(copy){
  if(SV_REPBUSY) return;
  SV_REPBUSY = true;
  var cp = svEl('svRepCopy'), send = svEl('svRepSend'), out = svEl('svRepOut');
  var label = cp ? cp.innerHTML : '';
  if(cp){ cp.disabled = true; cp.innerHTML = '生成中…'; }
  if(send) send.style.display = 'none';
  var noteEl = svEl('svRepNote');
  var note = noteEl ? noteEl.value : '';
  var done = function(rep){
    SV_REPBUSY = false;
    if(cp){ cp.disabled = false; cp.innerHTML = label; }
    if(!rep){
      if(out) out.textContent = '采集失败，请稍后重试；多次失败可直接微信找大叔人工协助。';
      toast('生成报告失败，请稍后重试', 'warn');
      return;
    }
    SV.report = rep;
    if(out) out.textContent = rep;
    if(send) send.style.display = '';
    if(!copy){ toast('报告已生成，可手动选中复制', 'ok'); return; }
    svCopy(rep, '报告已复制，粘贴到微信发给大叔即可');
  };
  if(!hasGo()){ done(''); return; }
  callGo('BuildDiagReport', note).then(function(rep){ done(rep || ''); })
    .catch(function(){ done(''); });
}

function svCopy(txt, okMsg){
  if(!hasGo()){ toast('后端未连接，请手动复制：' + txt, 'warn'); return; }
  callGo('CopyText', txt).then(function(err){
    if(err) toast('复制失败：' + err + ' —— 可手动输入 ' + txt, 'warn');
    else toast(okMsg, 'ok');
  }).catch(function(e){ toast('复制失败：' + String(e), 'warn'); });
}

/* ── 绑定：事件委托挂 #content（换页只换 innerHTML，元素会重建） ───────────── */
function bindSvc(){
  if(typeof SV.page === 'undefined') return;
  if(SV.state !== 'ready' && !SV.loading) loadSvc();
  var c = document.getElementById('content');
  if(!c || c.dataset.svcBound === '1') return;
  c.dataset.svcBound = '1';
  c.addEventListener('click', svClick);
}
window.bindSvc = bindSvc;

function svClick(e){
  var t = e.target;
  if(!t || !t.closest) return;
  var a = t.closest('[data-url]');
  if(a){ svOpen(a.getAttribute('data-url')); return; }
  var b = t.closest('[data-svcact]');
  if(!b) return;
  var act = b.getAttribute('data-svcact');
  if(act === 'hire') svcHire();
  else if(act === 'report') svcGenReport();
  else if(act === 'retry'){ SV.state = 'loading'; loadSvc(); if(typeof render === 'function') render(); }
}

/* 外链交给系统浏览器。🔴 别用 callGo('OpenURL') —— 后端没这个方法（2026-10-13 核对时发现，
   前端 78 个调用里唯一一个不存在的），调了必失败。走主文件的 window.tbOpen（内部是
   Wails runtime.BrowserOpenURL，失败会把地址复制到剪贴板）；浏览器直开时退化成新标签页。 */
function svOpen(url){
  if(!url) return;
  if(typeof window.tbOpen === 'function'){ window.tbOpen(url); return; }
  try{ window.open(url, '_blank', 'noopener'); }
  catch(_){ toast('打开失败，地址是：' + url, 'warn'); }
}
