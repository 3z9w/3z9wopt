/* ══════════════════════════════════════════════════════════════════════════
   关于 · 独立模块（design/about.js）· 2026-10-13 按 v2 老版本原样移植
   ---------------------------------------------------------------------------
   版面照 v2（frontend/dist/app.js 第 5591~5686 行）：
     hero（图标 + 名称 + 版本 + 一句话 + 四个动作）
     → 三张卡：软件信息 / 联系作者·微信群 / QQ 交流群
     → 底部一条温馨小提示

   只换了皮：配色、圆角、描边全走 v1.1 变量，跟服务大厅同一套语言。
   数据全部来自后端 GetAboutInfo()：版本 / 构建日期 / 官网 / Bug 入口 /
   微信号 / 微信群码 / QQ 群号 / QQ 群码 —— 页面上不写死任何一项。

   ⚠️ 对外入口全部挂 window：loadAbout / renderAboutPane / ABOUT
      （命令行读 window.ABOUT.bug 当反馈入口，别用裸变量）。
   ⚠️ 2026-10-13：关于**不再是一整页**，只作设置面板的一面，整页版 renderAbout / bindAbout 已删。

   2026-10-13 补：renderAboutPane / abClick 也挂 window ——
     设置弹窗改成「左导航 + 右面板」后，关于面板要用同一份渲染与点击处理。
     renderAboutPane 不带 page-head（面板里已经有分类标题，再叠一个页头就重复了）。
   ══════════════════════════════════════════════════════════════════════════ */

var AB = {state:'loading', err:'', info:null};
window.AB = AB;
window.ABOUT = null;

var AB_LOGO = 'data:image/png;base64,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';

var AB_ICO = {globe:'box', wrench:'gear', help:'info', user:'monitor', star:'sparkle'};
function abIco(k, s){ return ico(AB_ICO[k] || k || 'box', s || 16); }

function abEsc(s){
  return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
    return {'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;'}[c];
  });
}
function abEl(id){ return document.getElementById(id); }
/* 网址去掉协议和尾斜杠，信息行里只显示域名（https://opt.3z9w.com/ → opt.3z9w.com） */
function abHost(u){
  return String(u == null ? '' : u).replace(/^[a-zA-Z]+:\/\//, '').replace(/\/+$/, '') || '—';
}
/* 页面标记打在 page-head 上：loading 态也有它，任何状态下都能认出"我在关于页" */
function abOnPage(){ return !!document.querySelector('[data-abpage]'); }

function abAfter(){
  if(abOnPage() && typeof render === 'function') render();
  /* 设置弹窗里那个「关于」面板也吃这份数据 —— 走 window.setIsOpen 这个桥，
     不直接摸主文件的 let SET_OPEN（那是 let 声明的，不挂 window，够不着也不该够）。 */
  if(window.setIsOpen && window.setIsOpen() && typeof paintSettings === 'function') paintSettings();
}

function loadAbout(){
  if(!hasGo()){
    if(inWails()){ AB.state = 'failed'; AB.err = '后端连接还没就绪（window.go 为空）'; }
    abAfter();
    return;
  }
  AB.state = 'loading';
  callGo('GetAboutInfo').then(function(r){
    AB.info = r || {};
    AB.state = 'ready';
    window.ABOUT = AB.info;
    abAfter();
  }).catch(function(e){
    AB.state = 'failed'; AB.err = String(e);
    abAfter();
  });
}
window.loadAbout = loadAbout;

/* ── 渲染 ─────────────────────────────────────────────────────────────────── */

/* 设置弹窗里那个「关于」面板：同样的三卡版式，但不要本文件的 page-head ——
   面板标题由弹窗的导航项给出，再叠一层「关于 / 版本信息 · 联系作者」是重复。
   ⚠️ loading / failed 两态也要写清楚，不许甩空白（读不到就是读不到）。 */
function renderAboutPane(){
  if(AB.state === 'failed'){
    return '<div class="card"><div class="empty" style="padding:40px 20px">'
      + '<div class="empty-ico" style="color:var(--danger)">' + ico('warn', 28) + '</div>'
      + '<div class="empty-t">没能读到版本信息</div><div class="empty-d">' + abEsc(AB.err) + '</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      + '<button class="btn primary" data-abact="retry">' + ico('refresh', 16) + '重新读取</button>'
      + '</div></div></div>';
  }
  if(AB.state !== 'ready'){
    return '<div class="card"><div class="empty" style="padding:48px 20px">'
      + '<div class="empty-ico" style="color:var(--info)">' + ico('refresh', 28) + '</div>'
      + '<div class="empty-t">正在读版本信息…</div></div></div>';
  }
  /* 面板比整页窄（撑死 620px），三列卡片会挤成一团 —— 这里降成两列 */
  return '<div class="ab-pane">' + abBodyHtml() + '</div>';
}
window.renderAboutPane = renderAboutPane;

function abBodyHtml(){
  var a = AB.info || {};
  var ver = a.version || 'v1.1.0';
  var bd = (a.buildDate && a.buildDate !== 'unknown') ? a.buildDate : '';
  /* 构建号（20261019.2）比日期精确：同一天可能发好几次，日期分不出来。
     有构建号就用它，没有（日常构建）才退回日期。 */
  var bn = a.buildNum || '';
  var bl = bn || bd;
  var wxid = a.wxid || 'RESBJD';
  var wxQr = a.wxGroupQr || a.wxQr || '';
  var qq = a.qq || '', qqQr = a.qqQr || '';

  var acts = '<button class="btn sm" data-abact="site">' + abIco('globe', 13) + '访问官网</button>'
    + '<button class="btn sm" data-abact="publish">' + abIco('box', 13) + '发布页</button>'
    + '<button class="btn sm" data-abact="bug">' + abIco('warn', 13) + 'Bug 反馈</button>';
  if(typeof openPraise === 'function'){
    acts += '<button class="btn sm" data-abact="praise">' + abIco('heart', 13) + '赞赏支持</button>';
  }
  acts += '<button class="btn sm primary" data-abact="svc">' + abIco('message', 13) + '远程服务</button>';

  var qrBox = function(src, alt){
    if(!src) return '';
    return '<img src="' + abEsc(src) + '" alt="' + abEsc(alt) + '" onerror="this.parentNode.style.display=\'none\'">';
  };

  return '<div class="card ab-hero">'
    + (AB_LOGO ? '<img class="ab-logo" src="' + abEsc(AB_LOGO) + '" alt="3Z9W">' : '')
    + '<div class="ab-name">无名大叔 · 系统优化工具箱</div>'
    + '<div class="ab-ver">' + abEsc(ver) + (bl ? ' · 构建 ' + abEsc(bl) : '') + ' · 本地运行</div>'
    + '<p class="ab-desc">体检、清理、优化、修复，一台机器全搞定；自己搞不定的，一键找大叔远程修。</p>'
    + '<div class="ab-acts">' + acts + '</div>'
    + '</div>'

    + '<div class="ab-grid">'

    + '<div class="card ab-card">'
    +   '<div class="ab-t">' + abIco('info', 15) + '软件信息</div>'
    +   '<div class="ab-row"><span>版本</span><b>' + abEsc(ver) + '</b></div>'
    +   '<div class="ab-row"><span>' + (bn ? '构建号' : '构建日期') + '</span><b>' + abEsc(bl || '—') + '</b></div>'
    +   '<div class="ab-row"><span>技术架构</span><b>Go + Wails · 本地运行</b></div>'
    +   '<div class="ab-row"><span>官方网站</span><b>' + abEsc(abHost(a.site)) + '</b></div>'
    +   '<div class="ab-row"><span>发布页</span><b>' + abEsc(abHost(a.publish)) + '</b></div>'
    +   '<div class="ab-row"><span>数据处理</span><b>全部在本机完成，不上传</b></div>'
    + '</div>'

    + '<div class="card ab-card">'
    +   '<div class="ab-t">' + abIco('message', 15) + '联系作者 · 微信群</div>'
    +   (wxQr ? '<div class="ab-qr">' + qrBox(wxQr, '微信群二维码') + '</div>' : '')
    +   '<div class="ab-qr-tip">' + (a.wxGroupQr ? '扫码进微信群，电脑问题直接问' : '扫码加作者微信') + '</div>'
    +   '<div class="ab-wxid">微信号 <b>' + abEsc(wxid) + '</b>'
    +     '<button class="btn sm" data-abact="copywx">' + abIco('copy', 13) + '复制</button></div>'
    + '</div>'

    + '<div class="card ab-card">'
    +   '<div class="ab-t">' + abIco('help', 15) + 'QQ 交流群</div>'
    +   (qqQr ? '<div class="ab-qr">' + qrBox(qqQr, 'QQ 群二维码') + '</div>' : '')
    +   '<div class="ab-qq"><span>群号</span><b>' + abEsc(qq || '—') + '</b></div>'
    +   '<div class="ab-qr-tip">加群备注「优化」，反馈与答疑都在群里</div>'
    +   '<div class="ab-wxid"><button class="btn sm" data-abact="copyqq">' + abIco('copy', 13) + '复制群号</button></div>'
    + '</div>'

    + '</div>'

    + '<div class="card ab-note">' + abIco('shield', 16)
    + '<span>温馨小提示：优化会顺手动一动系统设置。要是怕哪步不合心意，先去「工具库」存个系统还原点当后悔药，随时能一键回到从前，权当啥也没发生过～</span></div>';
}

/* ── 绑定 ─────────────────────────────────────────────────────────────────── */

function abClick(e){
  var t = e.target;
  if(!t || !t.closest) return;
  var b = t.closest('[data-abact]');
  if(!b) return;
  var a = AB.info || {};
  var act = b.getAttribute('data-abact');
  if(act === 'site') abOpen(a.site || 'https://www.3z9w.com');
  else if(act === 'publish') abOpen(a.publish || a.site || 'https://opt.3z9w.com/');
  else if(act === 'bug') abOpen(a.bug || a.site || 'https://www.3z9w.com/1892.html#comments');
  else if(act === 'praise'){ if(typeof openPraise === 'function') openPraise(); }
  else if(act === 'svc'){ if(typeof go === 'function') go('svc'); }
  else if(act === 'copywx') abCopy(a.wxid || 'RESBJD', '微信号已复制，去微信添加好友吧');
  else if(act === 'copyqq') abCopy(a.qq || '', 'QQ 群号已复制，去 QQ 搜索群号加群');
  else if(act === 'retry'){ AB.state = 'loading'; loadAbout(); if(typeof render === 'function') render(); }
}
window.abClick = abClick;

function abCopy(txt, okMsg){
  if(!txt){ toast('没有可复制的内容', 'warn'); return; }
  if(!hasGo()){ toast('后端未连接，请手动复制：' + txt, 'warn'); return; }
  callGo('CopyText', txt).then(function(err){
    if(err) toast('复制失败：' + err + ' —— 可手动输入 ' + txt, 'warn');
    else toast(okMsg, 'ok');
  }).catch(function(e){ toast('复制失败：' + String(e), 'warn'); });
}

/* 🔴 后端**没有** OpenURL 这个绑定方法（2026-10-13 全量核对：前端 78 个 callGo 里
   就这一个后端不存在），以前 callGo('OpenURL') 必失败 → 所有外链都打不开。
   正路是 Wails 自带的 window.runtime.BrowserOpenURL —— 主文件里已经封成 window.tbOpen，
   这里直接复用；浏览器直开原型（没有 runtime、也没有 tbOpen）退化成新标签页。 */
function abOpen(url){
  if(!url) return;
  if(typeof window.tbOpen === 'function'){ window.tbOpen(url); return; }
  try{ window.open(url, '_blank', 'noopener'); }
  catch(_){ toast('打开失败，地址是：' + url, 'warn'); }
}
