/* ══════════════════════════════════════════════════════════════════════════
   启动项管理 · 独立模块（design/startup.js）
   ---------------------------------------------------------------------------
   功能与交互 100% 照老版本（v2 启动项管理 2.0）搬过来：
     · 引导卡只留三条要点（管什么 / 关掉后 / 改错了），不再重复页头副标题里说过的话
     · 一键优化（勾选清单 → 逐项禁用 → 进度 → 完成，可随时在优化记录里恢复）
     · 三分类分段（启动项 / 服务项 / 计划任务，带计数）
     · 可排序表头（名称 / 优化建议 / 当前状态）
     · 行菜单：忽略此项 · 打开所在位置 · 复制完整命令行
     · 优化记录弹窗（单项恢复 + 全部恢复）· 忽略区弹窗（取消忽略）
     · 图标异步取（GetAppIcon），取不到用首字母彩色头像（每一项都得有图标）
   后端接口本版 startup.go 与老版本完全同名，一行后端都没改。
   视觉按 1.1 重写（玻璃卡 + 蓝为主，橙红只用在「该动手的那一行」）。
   ⚠️ 状态与函数一律挂 window（加载器是 IIFE 包着的 eval，见 ui-prototype.html 的
      「启动项管理页独立文件加载器」），否则 PAGES 引用不到 → 整页白屏。
   ══════════════════════════════════════════════════════════════════════════ */

/* diag：各分类的"读取失败原因"（空 = 都读成功了）。**0 项不等于没读到**，
   历史上计划任务读失败被显示成「计划任务 0」，用户以为自己机器上本来没有。
   admin：是否管理员 —— 非管理员改不动服务项 / 全机启动项，得先说清楚。 */
var SU_S = {loaded:false, loading:false, tab:'run', items:[], hide:false, showSys:false, sort:'advice', err:'', diag:{}, admin:true};
var SU_KIND = {run:'启动项', svc:'服务项', task:'计划任务'};
/* 分类分段：[图标, 名称, 说明] —— 图标名必须是主文件图标池里有的 */
var SU_META = {
  run:  ['box',   '启动项',   '开机自动运行的程序'],
  svc:  ['cpu',   '服务项',   '后台自动运行的服务'],
  task: ['clock', '计划任务', '定时自动执行的任务']
};
var SU_FB_HUES = [212, 262, 8, 160, 32, 330, 190];
var SU_ICON = {};   /* iconKey → data URL；重画时同步贴回，避免"重扫后图标先消失再慢慢回来" */

/* 建议禁用的 = 真正「该动手」的那批（已停用的不算） */
function suAdv(){
  return SU_S.items.filter(function(i){ return i.advice === 'disable' && i.enabled; });
}
/* 建议删除的 = 程序已经不在了的无效项。
   它和「建议禁止」是两个动作：禁用可恢复，删除不可恢复，所以不进一键优化清单。 */
function suDel(){
  return SU_S.items.filter(function(i){ return i.advice === 'delete'; });
}
/* 排序权重：建议删除 > 建议禁止 > 保持现状（和 Go 侧 adviceRank 一致） */
function suRank(it){
  return it.advice === 'delete' ? 0 : (it.advice === 'disable' ? 1 : 2);
}
function suEsc(s){ return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
  return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];
});}
function suEl(id){ return document.getElementById(id); }
function suEach(sel, fn){
  var l = document.querySelectorAll(sel);
  for(var i = 0; i < l.length; i++) fn(l[i], i);
}
function suTime(t){
  try{ return new Date(t * 1000).toLocaleString('zh-CN', {hour12:false}); }catch(e){ return ''; }
}
/* 数据是首页（体检里的启动项那一行）和「操作记录一键还原」的唯一来源，
   每次拿到新数据都要同步回主文件的 SU，别让两处各说各话。 */
function suSyncGlobal(){
  SU = SU_S.items;
  if(typeof cur !== 'undefined' && cur === 'home' && typeof syncHomeLive === 'function') syncHomeLive();
}

/* ── 页面骨架 ───────────────────────────────────────────────────────────── */
function renderStartup(){
  /* 进页先校验分类：SU_S.tab 一旦是无效值，三个分段全不高亮、列表永远空白，
     看着就像"这一页坏了"。不信任内存里的值，回落到第一个分类。 */
  if(!SU_META[SU_S.tab]) SU_S.tab = Object.keys(SU_KIND)[0];
  return '<div class="page-head"><div class="page-title">启动项管理</div>'
    + '<div class="page-sub">管理开机自启动，把不该开机运行的程序关掉，提升系统开机速度！</div></div>'
    + '<div class="su-guide">'
    +   '<div class="su-guide-ico">' + ico('shield', 22) + '</div>'
    +   '<div class="su-guide-main">'
    +     '<ul class="su-guide-grid">'
    +       '<li title="只管开机后要不要自动启动；想用时手动打开，功能一点不少"><span class="k">管什么</span><span class="v">只管开机是否自动运行，功能一点不少</span></li>'
    +       '<li title="开机更快、后台更干净、内存占用更低；代价是它不再自动运行"><span class="k">关掉后</span><span class="v">开机更快、后台更干净，但不再自动启动</span></li>'
    +       '<li title="开关拨回来即可恢复；「优化记录」里也能一键还原"><span class="k">改错了</span><span class="v">开关拨回来即可，记录里还能一键恢复</span></li>'
    +     '</ul>'
    +   '</div>'
    +   '<div class="su-guide-act">'
    +     '<button class="btn su-cta" id="suOptBtn">' + ico('bolt', 15) + '<span>一键优化</span><b id="suOptN">0</b></button>'
    +     '<div class="su-cta-hint" id="suOptHint">正在检测…</div>'
    +   '</div>'
    + '</div>'
    + '<div id="suTip"></div>'
    + '<div class="card su-bar">'
    +   '<div class="su-seg" id="suTabs"></div>'
    +   '<div class="su-bar-act">'
    +     '<button class="btn sm" id="suReload" title="重新读取全部启动项（含最新安装 / 卸载的变化）">' + ico('refresh', 14) + '<span>重新扫描</span></button>'
    +     '<button class="btn sm" id="suLogBtn" title="查看最近的启用 / 禁用操作，可一键恢复">' + ico('doc', 14) + '<span>优化记录</span> <b id="suLogN">0</b></button>'
    +     '<button class="btn sm" id="suIgnBtn" title="被忽略、不再出现在列表里的启动项">' + ico('fold', 14) + '<span>忽略区</span> <b id="suIgnN">0</b></button>'
    +     '<label class="btn sm" title="只看当前还在运行的启动项"><input type="checkbox" id="suHideChk"><span>隐藏已禁用</span></label>'
    +     '<label class="btn sm" title="默认只看第三方软件装的项；勾上把 Windows 自带的系统服务 / 系统任务也列出来"><input type="checkbox" id="suSysChk"><span>显示系统自带</span></label>'
    +   '</div>'
    + '</div>'
    + '<div id="suList"></div>';
}

/* ── 载入 ───────────────────────────────────────────────────────────────── */
function suPaintLoading(){
  var h = suEl('suList');
  if(h) h.innerHTML = '<div class="su-norow"><span class="su-spin" style="display:inline-block;vertical-align:-3px;margin-right:8px"></span>正在扫描启动项（Run / 服务 / 计划任务）…</div>';
}
/* 重扫进行中：按钮转圈并禁用，防连点 */
function suScanBusy(on){
  suEach('#suReload, #suOptBtn', function(b){ b.disabled = !!on; b.classList.toggle('busy', !!on); });
}

function suLoad(){
  if(SU_S.loading) return;            /* 防并发重复扫描（连点刷新 / 弹窗内恢复后） */
  SU_S.loading = true; SU_S.loaded = false;
  var hint = suEl('suOptHint'); if(hint) hint.textContent = '正在检测…';
  /* 重扫时保留旧列表，只让按钮转圈 —— 不清空白等 */
  if(!SU_S.items.length) suPaintLoading();
  suScanBusy(true);
  callGo('ListStartup').then(function(items){
    SU_S.loading = false; SU_S.loaded = true; SU_S.err = '';
    SU_S.items = items || [];
    suScanBusy(false);
    suSyncGlobal();
    suLoadDiag();
    suBadges();                                              /* 跟列表无关，切走了也要更新 */
    if(typeof cur !== 'undefined' && cur !== 'sec') return;   /* 已经切走了就别渲染 */
    suRenderAll();   /* 内部 suRenderList 已经会把图标贴回来，不用再补一次 */
  }).catch(function(e){
    SU_S.loading = false; SU_S.loaded = false; SU_S.err = String(e);
    suScanBusy(false);
    var h = suEl('suList');
    if(h && typeof cur !== 'undefined' && cur === 'sec'){
      h.innerHTML = '<div class="su-norow">扫描失败：' + suEsc(SU_S.err) + ' —— 点「重新扫描」重试</div>';
    }
  });
}
/* 读取状态 + 是否管理员。两个都是"读不到也不该拦住页面"的辅助信息，失败就静默。 */
function suLoadDiag(){
  callGo('GetStartupDiag').then(function(d){
    var old = JSON.stringify(SU_S.diag || {});
    SU_S.diag = d || {};
    if(typeof cur !== 'undefined' && cur === 'sec'){
      suRenderTabs();
      /* 只在"读取失败原因"确实变了才重画列表：诊断几乎总是紧跟着列表回来，
         无条件重画会把刚建好的行整段拆掉重建，图标队列跟着重启 —— 观感上就是
         「图标先空着，过一会儿才出现」。 */
      if(JSON.stringify(SU_S.diag) !== old) suRenderList();
    }
  }).catch(function(){ SU_S.diag = {}; });
  callGo('IsElevated').then(function(v){
    SU_S.admin = v !== false;
    if(typeof cur !== 'undefined' && cur === 'sec') suRenderTip();
  }).catch(function(){ SU_S.admin = true; });
  /* 页面重绘后内存里的 showSys 会归零，但后端还记得 —— 不回读就会出现
     「列表里明明有系统项、勾选框却是空的」。 */
  callGo('GetStartupShowSystem').then(function(v){
    SU_S.showSys = !!v;
    suSyncSysChk();
  }).catch(function(){});
}
/* 非管理员提示：服务项 / 全机启动项 / 系统计划任务必须提权才能改，不先说清楚就是"拨了报错" */
function suRenderTip(){
  var host = suEl('suTip');
  if(!host) return;
  host.innerHTML = SU_S.admin ? '' :
    '<div class="su-tipbar">' + ico('shield', 14)
    + '<span>当前不是管理员：<b>服务项</b>和<b>全机启动项</b>改不动（开机自用类不受影响）。'
    + '需要改的话，退出后「以管理员身份运行」本程序再进来。</span></div>';
}

/* 强制重扫：绕过后端 10 分钟缓存。
   重扫本身要花几百毫秒（几百条服务 / 任务要重新读），先让按钮转圈，
   否则从点到按钮有反应之间有一段"什么都没发生"的空窗。 */
function suForceLoad(){
  suScanBusy(true);
  callGo('ListStartupForce').then(function(){ suLoad(); },
    function(){ suLoad(); });
}

/* ── 渲染 ───────────────────────────────────────────────────────────────── */
/* 一键优化按钮：没事时弱化成普通按钮（别抢注意力），真检出可优化项才橙红高亮 */
function suOptBar(){
  var adv = suAdv();
  var b = suEl('suOptBtn');
  if(b){
    b.classList.toggle('hot', adv.length > 0);
    b.classList.toggle('muted', adv.length === 0);
    b.title = adv.length
      ? ('已发现 ' + adv.length + ' 项建议禁用的启动项，点这里批量处理（只取消开机自启，可随时恢复）')
      : '当前没有建议禁用的项目；你也可以手动拨动下方任意开关';
  }
  var n = suEl('suOptN'); if(n) n.textContent = adv.length;
  var hint = suEl('suOptHint');
  if(hint){
    /* 「建议优化」和「建议删除」分开计数：删除是不可恢复的，不能混进一键优化的数量里，
       但也不能不告诉用户 —— 否则这些尸体条目会一直躺在列表里没人管。 */
    var dl = suDel().length;
    hint.textContent = (adv.length ? (adv.length + ' 项建议禁止') : '当前无需处理')
      + (dl ? (' · ' + dl + ' 项无效（建议删除）') : '');
    hint.classList.toggle('hot', adv.length > 0 || dl > 0);
  }
}

function suRenderTabs(){
  var box = suEl('suTabs');
  if(!box) return;
  box.innerHTML = Object.keys(SU_KIND).map(function(k){
    var n = SU_S.items.filter(function(i){ return i.kind === k; }).length;
    var m = SU_META[k];
    var bad = SU_S.diag[k];
    /* 这一类压根没读到时不能显示 0（那是"这台机没有"的意思），显示 ! 并把原因挂在 title 上 */
    var badge = bad
      ? '<span class="n bad" title="这一项没能读到：' + suEsc(bad) + '">!</span>'
      : '<span class="n">' + n + '</span>';
    return '<button class="' + (SU_S.tab === k ? 'on' : '') + '" data-su-tab="' + k + '" title="' + m[2] + '">'
      + ico(m[0], 15) + '<span>' + m[1] + '</span>' + badge + '</button>';
  }).join('');
}

/* 排序取值：advice（有问题在前，默认）· name · state */
function suRows(){
  var rows = SU_S.items.filter(function(i){ return i.kind === SU_S.tab; });
  if(SU_S.hide) rows = rows.filter(function(i){ return i.enabled; });
  var byName = function(a, b){ return String(a.name).localeCompare(String(b.name), 'zh'); };
  var s = SU_S.sort || 'advice';
  return rows.slice().sort(function(a, b){
    if(s === 'name') return byName(a, b);
    if(s === 'state') return ((b.enabled ? 1 : 0) - (a.enabled ? 1 : 0)) || byName(a, b);
    return (suRank(a) - suRank(b)) || ((b.enabled ? 1 : 0) - (a.enabled ? 1 : 0)) || byName(a, b);
  });
}

function suHeadHtml(){
  var s = SU_S.sort || 'advice';
  var on = function(k){ return s === k ? ' on' : ''; };
  return '<div class="su-head">'
    + '<span class="su-hico"></span>'
    + '<button class="su-h su-h-name' + on('name') + '" data-su-sort="name" title="按名称排序">程序名称</button>'
    + '<button class="su-h su-h-adv' + on('advice') + '" data-su-sort="advice" title="可手动优化的项目排在最前">优化建议</button>'
    + '<button class="su-h su-h-state' + on('state') + '" data-su-sort="state" title="按当前启用状态排序">当前状态</button>'
    + '<span class="su-hico"></span><span class="su-hico"></span></div>';
}

function suRowHtml(it){
  /* 三档建议：建议删除 / 建议禁止 / 保持现状；已停用的单独一档 */
  var advB = it.advice === 'delete'
    ? '<span class="b del" title="' + suEsc(it.why || '这个启动项指向的程序已经不在了，建议清理掉') + '">建议删除</span>'
    : (it.advice === 'disable'
      ? '<span class="b yes" title="' + suEsc(it.why || '建议禁用：只取消开机自启，不影响软件功能，随时可恢复') + '">建议禁止</span>'
      : (!it.enabled
        ? '<span class="b no" title="当前是停用状态，需要时可随时恢复启用">已停用</span>'
        : '<span class="b no" title="' + suEsc(it.why || '暂无优化建议，保持现状即可') + '">保持现状</span>'));
  var sysTag = it.sys ? '<i class="su-tag sys" title="系统自带组件，保持现状更稳妥">系统</i>' : '';
  var sub = [it.desc || it.pub, (it.pub && it.desc) ? it.pub : '', it.loc].filter(Boolean).join(' · ');
  /* 「保持现状」也要说清为什么保留：只给开关不给理由，新手不知道该不该关 */
  /* 「保持现状」也要说清为什么保留：只给开关不给理由，新手不知道该不该关。
     无效项即使已停用也要把理由写出来 —— 那是唯一告诉用户「这条可以直接删」的地方。 */
  var whyRow = (it.advice !== 'disable' && it.why && (it.enabled || it.advice === 'delete'))
    ? '<div class="su-why" title="' + suEsc(it.why) + '">' + ico('check', 12) + '<span>' + suEsc(it.why) + '</span></div>' : '';
  var hot = it.advice === 'delete' || (it.advice === 'disable' && it.enabled);
  return '<div class="su-row' + (hot ? ' su-hot' : '') + '" data-id="' + suEsc(it.id) + '">'
    /* ⚠️ 属性名必须带 su- 前缀：主文件 render() 会对 #content 里的 [data-ico] 调 paintIcons，
       那是"内置图标名"的钩子。这里存的是 iconKey（w|C:\...\a.exe|0），撞名后
       paintIcons 会把 P[iconKey] 查不到 → 画成方框，还顺手打上 data-done —— 真图标再也不会贴。 */
    + '<div class="su-ico" data-su-ico="' + suEsc(it.iconKey || '') + '">' + suLetterHtml(it.name) + '</div>'
    + '<div class="su-main">'
    +   '<div class="su-nm" title="' + suEsc(it.cmd || '') + '"><span class="su-nmt">' + suEsc(it.name) + '</span>' + sysTag + '</div>'
    +   '<div class="su-sub" title="' + suEsc(it.cmd || it.loc) + '">' + suEsc(sub || '无描述信息') + '</div>'
    +   whyRow
    + '</div>'
    + '<div class="su-adv">' + advB + '</div>'
    + '<div class="su-state ' + (it.enabled ? 'on' : 'off') + '"><i></i>' + (it.enabled ? '已启用' : '已禁用') + '</div>'
    + '<label class="su-switch" title="' + (it.enabled ? '点击禁用（可恢复）' : '点击恢复启用') + '">'
    +   '<input type="checkbox" data-su-toggle="' + suEsc(it.id) + '"' + (it.enabled ? ' checked' : '') + '><span></span></label>'
    + '<button class="su-more" data-su-more="' + suEsc(it.id) + '" title="更多操作">⋮</button>'
    + '</div>';
}

function suRenderList(){
  var host = suEl('suList');
  if(!host) return;
  var rows = suRows();
  if(rows.length){
    host.innerHTML = '<div class="su-list">' + suHeadHtml() + rows.map(suRowHtml).join('') + '</div>';
    /* 图标必须跟着重画走：以前只有"扫描完成"那条路径会贴图标，
       其它重画路径（诊断回来、切页签、改排序）都漏了 → 图标消失且不回来 */
    suIcons(host);
    return;
  }
  var bad = SU_S.diag[SU_S.tab];
  host.innerHTML = '<div class="card"><div class="su-norow' + (bad ? ' bad' : '') + '">'
    + (bad
        ? ico('shield', 15) + '<div><b>这一类的启动项没能读到</b><br>' + suEsc(bad)
          + '<br>先看看另外两类，或点「重新扫描」再试一次</div>'
        : (SU_S.hide ? '该分类下没有已启用的项目'
            : (SU_S.showSys ? '没有发现项目'
                : '没有发现第三方项目（Windows 自带的已隐藏，勾选「显示系统自带」可查看）')))
    + '</div></div>';
}

function suRenderAll(){
  if(typeof cur !== 'undefined' && cur !== 'sec') return;
  suSyncSysChk(); suOptBar(); suRenderTabs(); suRenderList();
}
/* 「显示系统自带」勾选框的状态从数据来：整页重绘会把 input 打回未勾选，
   不补一下就会出现「明明在显示系统项、框却是空的」 */
function suSyncSysChk(){
  var c = suEl('suSysChk');
  if(c) c.checked = !!SU_S.showSys;
}

/* 切「显示系统自带」：后端缓存本来就是全量，改完标记重新取一次即可（不打转圈、不重扫）。 */
function suToggleSys(on){
  SU_S.showSys = !!on;
  callGo('SetStartupShowSystem', !!on).then(function(){
    return callGo('ListStartup');
  }).then(function(items){
    SU_S.items = items || [];
    suSyncGlobal();
    suRenderAll();
  }).catch(function(){
    /* 切不动就把勾选框拨回去，别让界面和实际口径各说各话 */
    SU_S.showSys = !on; suSyncSysChk();
  });
}

/* ── 图标（异步取，取不到用首字母头像）───────────────────────────────────── */
/* 首字母彩色头像。列表第一帧就用它占位 —— 以前占位是统一的灰色方框/CPU/时钟图标，
   一屏全是同一个灰字形，看着像"图标没读出来"；换成按名字取色的字母块，
   真图标随后淡入，观感上从第一帧起就是"已经画好了"。 */
function suLetterHtml(nm){
  nm = String(nm == null ? '' : nm).trim() || '?';
  var ch = nm.charAt(0).toUpperCase();
  var h = 0;
  for(var i = 0; i < nm.length; i++) h = (h * 31 + nm.charCodeAt(i)) >>> 0;
  var hue = SU_FB_HUES[h % SU_FB_HUES.length];
  return '<span class="fb-l" style="background:hsl(' + hue + ' 70% 92%);color:hsl(' + hue + ' 65% 40%)">' + suEsc(ch) + '</span>';
}
function suFbLetter(el){
  var cell = el.closest ? el.closest('.su-row, .su-optrow') : null;
  var nm = (cell && cell.querySelector('.su-nmt') ? cell.querySelector('.su-nmt').textContent : '') || '?';
  return suLetterHtml(nm);
}
function suPaintIcon(el, url){
  if(!url){ el.innerHTML = suFbLetter(el); return; }
  var img = document.createElement('img');
  img.onload = function(){ img.classList.add('in'); };
  img.src = url;
  el.innerHTML = '';
  el.appendChild(img);
}
/* ⚠️ 图标结果按 iconKey 记在内存里，跨重渲染复用。
   以前只贴在 DOM 上：任何一次重画（重扫、切页签、改排序、诊断回来）都会把 <img> 冲掉，
   而重画的那条路径**没有再调 suIcons** —— 于是「重新扫描后图标消失，而且不会再回来」。
   现在重画时先用缓存同步贴回去，观感上图标根本不动。

   ⚠️⚠️ 队列按 iconKey 去重，**不用"代号"取消**（2026-10-19 第二次修）：
   上一版靠 SU_S.iconGen 作废上一批请求，但只要有两条重画路径前后脚各调一次
   suIcons（suRenderList 内部一次 + 外面补一次），后一次就会把前一次的请求全部作废，
   而前一次已经发出的那几个图标再也没人贴 —— 实测 6 个只出现 2 个。
   现在改为：同一 iconKey 只有一条请求，等结果回来的元素全挂在 SU_ICON_PEND 上，
   回来时一次性贴给所有还在页面里的元素（含重画后的新节点），不存在"作废"这回事。 */
var SU_ICON_PEND = {};    /* iconKey → 正在等这个图标的一组元素 */
var SU_ICON_QUEUE = [];   /* 还没发出去的 key（先进先出） */
var SU_ICON_BUSY = 0;
var SU_ICON_WORKERS = 4;  /* 串行取几十个图标会明显卡住观感 */

function suIcons(root){
  if(!root) return;
  var all = root.querySelectorAll('.su-ico[data-su-ico]');
  for(var i = 0; i < all.length; i++){
    var el = all[i];
    var key = el.getAttribute('data-su-ico');
    if(!key) continue;
    /* 取过的（包括"取不到" = 空串）直接贴，不再走一次 Go 往返 */
    if(Object.prototype.hasOwnProperty.call(SU_ICON, key)){
      suPaintIcon(el, SU_ICON[key]);
      continue;
    }
    if(!Object.prototype.hasOwnProperty.call(SU_ICON_PEND, key)){
      SU_ICON_PEND[key] = [];
      SU_ICON_QUEUE.push(key);
    }
    SU_ICON_PEND[key].push(el);
  }
  suIconPump();
}
function suIconPump(){
  while(SU_ICON_BUSY < SU_ICON_WORKERS && SU_ICON_QUEUE.length){
    (function(key){
      SU_ICON_BUSY++;
      var done = function(url){
        SU_ICON[key] = url || '';
        var wait = SU_ICON_PEND[key] || [];
        delete SU_ICON_PEND[key];
        for(var i = 0; i < wait.length; i++){
          /* 已经被下一次重画换掉的节点别再写（写了也看不见，白费一次重排） */
          if(wait[i].isConnected === false) continue;
          suPaintIcon(wait[i], url);
        }
        SU_ICON_BUSY--;
        suIconPump();
      };
      callGo('GetAppIcon', key).then(done, function(){ done(''); });
    })(SU_ICON_QUEUE.shift());
  }
}

/* ── 开关 ───────────────────────────────────────────────────────────────── */
function suToggle(cb, id, on){
  var it = null;
  SU_S.items.forEach(function(x){ if(x.id === id) it = x; });
  cb.disabled = true;
  /* ⚠️ 后端 SetStartupEnable 是 (string, error)：**成功时返回项目名**（非空！），
     失败走 Wails 的 reject —— 所以"返回值非空"不等于报错。
     早期写成 if(msg) 当错误处理 → 每次拨开关都被当成失败、开关弹回去还弹红色 toast（用户报"开关无法设置，报错"）。
     判断成败只看 then/catch，不看返回值。 */
  callGo('SetStartupEnable', id, on).then(function(){
    if(it) it.enabled = on;
    toast((it ? it.name : '项目') + ' 已' + (on ? '恢复启用' : '禁用'), on ? 'ok' : 'info');
    suSyncGlobal();
    suRenderAll();
    suBadges();          /* 单拨一次开关也进操作记录，角标得跟着走（不然和优化记录对不上） */
  }).catch(function(e){
    cb.checked = !on; cb.disabled = false;
    toast(String(e), 'danger');
  });
}

/* ── 行内菜单 ───────────────────────────────────────────────────────────── */
/* 从命令行里取程序路径，给「打开所在位置」用（explorer /select 必须拿到完整路径）。
   ⚠️ 原来是 /^[^ ]*\.exe/：带引号**且路径里有空格**的命令行
   （"C:\Program Files\a b\x.exe" -x，装在 Program Files 下的软件全是这种写法）
   匹配不到 → exe 为空 → 「打开所在位置」按钮直接被禁用。最常见的那种反而点不了。 */
function suExeOf(cmd){
  var s = String(cmd || '').trim();
  if(!s) return '';
  if(s.charAt(0) === '"'){
    var i = s.indexOf('"', 1);
    if(i > 1) return s.slice(1, i);
  }
  /* 没引号：截到第一个 .exe 之类的后缀为止（和后端 cmdToExe 的思路一致） */
  var m = s.match(/^\S+/);
  if(!m) return '';
  var t = m[0];
  var k = t.search(/\.(exe|com|bat|cmd|vbs|js|ps1|scr|msi)\b/i);
  return k >= 0 ? t.slice(0, k + 4) : t;
}
function suMenu(btn){
  suEach('.su-menu', function(m){ m.remove(); });
  var id = btn.getAttribute('data-su-more');
  var it = null;
  SU_S.items.forEach(function(x){ if(x.id === id) it = x; });
  if(!it) return;
  var exe = suExeOf(it.cmd);
  var m = document.createElement('div');
  m.className = 'su-menu';
  /* 2026-10-19：把「禁用」和「删除」分成两个动作。
     禁用只改状态、可恢复；删除动真格、不可恢复 —— 混在一个开关里，
     用户拨一下就可能永久丢掉一条启动项。服务不给删：删错一个服务可能让系统组件直接失效。 */
  var canDel = (it.kind !== 'svc');
  m.innerHTML = '<button data-act="ign">忽略此项</button>'
    + '<button data-act="open"' + (exe ? '' : ' disabled') + '>打开所在位置</button>'
    + '<button data-act="copy"' + (it.cmd ? '' : ' disabled') + '>复制完整命令行</button>'
    + '<button data-act="del"' + (canDel ? '' : ' disabled') + '>'
    + (canDel ? '彻底删除（不可恢复）' : '彻底删除（服务只能禁用）') + '</button>'
    + '<div class="su-menu-cmd">' + suEsc(it.cmd || it.loc) + '</div>';
  document.body.appendChild(m);
  var r = btn.getBoundingClientRect();
  m.style.top = Math.min(r.bottom + 6, window.innerHeight - m.offsetHeight - 10) + 'px';
  /* 夹到视口内：⋮ 在最左边几行时，原来算出来是负数，菜单会被截在窗口外 */
  m.style.left = Math.max(8, Math.min(r.left - 150, window.innerWidth - 230)) + 'px';
  m.querySelector('[data-act="ign"]').onclick = function(){
    m.remove();
    callGo('StartupIgnore', it.id, it.name, it.kind, it.desc || '', true).then(function(){
      toast('已忽略 —— 不再出现在列表和优化建议中', 'ok');
      suForceLoad();
    }).catch(function(e){ toast(String(e), 'danger'); });
  };
  m.querySelector('[data-act="open"]').onclick = function(){
    m.remove();
    callGo('SpaceOpen', exe).catch(function(){ toast('打开失败', 'danger'); });
  };
  m.querySelector('[data-act="copy"]').onclick = function(){
    m.remove();
    try{
      navigator.clipboard.writeText(it.cmd).then(function(){ toast('命令行已复制', 'ok'); },
        function(){ toast('复制失败', 'danger'); });
    }catch(e){ toast('复制失败', 'danger'); }
  };
  var delBtn = m.querySelector('[data-act="del"]');
  if(delBtn) delBtn.onclick = function(){
    m.remove();
    if(!canDel) return;
    askThenDo('彻底删除这个启动项？',
      '「' + suEsc(it.name) + '」删除后无法从这里恢复。'
      + '<br>如果只是想让它开机不运行，用左边的开关<b>禁用</b>就行 —— 禁用随时可以开回来。',
      '确认删除',
      function(){
        callGo('DeleteStartup', it.id).then(function(){
          toast('已删除「' + it.name + '」', 'ok');
          suForceLoad();
        }).catch(function(e){ toast(String(e), 'danger'); });
      });
  };
  setTimeout(function(){
    var close = function(ev){ if(!m.contains(ev.target)){ m.remove(); document.removeEventListener('click', close); } };
    document.addEventListener('click', close);
  }, 0);
}

/* ── 弹窗基础（本版 #modalBox + .modal.open，没有 modal(html) 这种封装）──── */
function suModal(html){
  var box = suEl('modalBox');
  if(!box) return;
  box.innerHTML = html;
  var mk = suEl('modal');
  if(mk) mk.classList.add('open');
}
function suModalClose(){ if(typeof closeModal === 'function') closeModal(); }

/* ── 优化记录（单项恢复 + 全部恢复）───────────────────────────────────── */
function suLogModal(){
  callGo('GetStartupLog').then(function(log){
    log = log || [];
    var canAll = log.some(function(op){ return op.act === 'disable'; });
    suModal('<div class="modal-t">' + ico('doc', 18) + '优化记录</div>'
      + '<div class="modal-d">共 ' + log.length + ' 条 · 最近的在前，禁用的可随时恢复</div>'
      + (canAll ? '<div class="su-log-top"><span>批量操作</span><button class="btn sm" id="suRestoreAll">' + ico('refresh', 14) + '全部恢复</button></div>' : '')
      + '<div class="su-loglist">'
      + (log.length ? log.map(function(op){
          return '<div class="su-logrow">'
            + '<span class="t">' + suTime(op.time) + '</span>'
            + '<span class="su-op ' + (op.act === 'disable' ? 'd' : 'e') + '">' + (op.act === 'disable' ? '禁用' : '启用') + '</span>'
            + '<span class="k">' + (SU_KIND[op.kind] || op.kind) + '</span>'
            + '<span class="n" title="' + suEsc(op.name) + '">' + suEsc(op.name) + '</span>'
            + (op.act === 'disable' ? '<button class="btn sm" data-su-restore="' + suEsc(op.id) + '">' + ico('refresh', 14) + '恢复</button>' : '')
            + '</div>';
        }).join('') : '<div class="su-norow">还没有操作记录</div>')
      + '</div>'
      + '<div class="modal-f"><button class="btn" data-act="c">关闭</button></div>');
    var c = suEl('modalBox').querySelector('[data-act="c"]');
    if(c) c.onclick = suModalClose;
    var ra = suEl('suRestoreAll');
    if(ra) ra.onclick = function(){
      ra.disabled = true; ra.textContent = '恢复中…';
      var ids = [], seen = {};
      log.forEach(function(op){ if(op.act === 'disable' && !seen[op.id]){ seen[op.id] = 1; ids.push(op.id); } });
      var fail = 0, k = 0;
      (function step(){
        if(k >= ids.length){
          suModalClose(); suForceLoad();
          toast(fail ? ('恢复完成，' + fail + ' 项失败（可能已被卸载）') : ('已恢复 ' + (ids.length - fail) + ' 项'), fail ? 'danger' : 'ok');
          return;
        }
        callGo('SetStartupEnable', ids[k++], true).then(function(){ step(); },
          function(){ fail++; step(); });
      })();
    };
    suEach('#modalBox [data-su-restore]', function(b){
      b.onclick = function(){
        b.disabled = true; b.textContent = '恢复中…';
        callGo('SetStartupEnable', b.getAttribute('data-su-restore'), true).then(function(){
          suModalClose(); suForceLoad(); toast('已恢复启用', 'ok');
        }).catch(function(e){ b.disabled = false; toast(String(e), 'danger'); });
      };
    });
  }).catch(function(e){ toast('读取失败：' + e, 'danger'); });
}

/* ── 忽略区 ─────────────────────────────────────────────────────────────── */
function suIgnoreModal(){
  callGo('GetStartupIgnored').then(function(ig){
    ig = ig || [];
    suModal('<div class="modal-t">' + ico('fold', 18) + '忽略区</div>'
      + '<div class="modal-d">已忽略 ' + ig.length + ' 项 —— 不再出现在列表中，也不参与优化建议</div>'
      + '<div class="su-loglist">'
      + (ig.length ? ig.map(function(g){
          return '<div class="su-logrow ign">'
            + '<span class="k">' + (SU_KIND[g.kind] || g.kind || '启动项') + '</span>'
            + '<span class="n" title="' + suEsc(g.desc || '') + '">' + suEsc(g.name || g.id) + '</span>'
            + '<button class="btn sm" data-su-unign="' + suEsc(g.id) + '">取消忽略</button></div>';
        }).join('') : '<div class="su-norow">暂无忽略任何启动项目</div>')
      + '</div>'
      + '<div class="modal-f"><button class="btn" data-act="c">关闭</button></div>');
    var c = suEl('modalBox').querySelector('[data-act="c"]');
    if(c) c.onclick = suModalClose;
    suEach('#modalBox [data-su-unign]', function(b){
      b.onclick = function(){
        b.disabled = true; b.textContent = '恢复中…';
        callGo('StartupIgnore', b.getAttribute('data-su-unign'), '', '', '', false).then(function(){
          suModalClose(); suForceLoad(); toast('已恢复显示', 'ok');
        }).catch(function(e){ b.disabled = false; toast(String(e), 'danger'); });
      };
    });
  }).catch(function(e){ toast('读取失败：' + e, 'danger'); });
}

/* ── 一键优化（勾选清单 → 逐项禁用 → 进度 → 完成）───────────────────────── */
function suOptModal(){
  var adv = suAdv();
  if(!adv.length){ toast('当前没有需要优化的项目', 'info'); return; }
  suModal('<div class="modal-t">' + ico('bolt', 18) + '一键优化 · ' + adv.length + ' 项</div>'
    + '<div class="modal-d">勾选要禁用的启动项 —— 只是不再开机自启，软件功能不受影响，随时可恢复</div>'
    + '<div class="su-optlist" id="suOptList">'
    + adv.map(function(it){
        var kindIco = it.kind === 'task' ? 'clock' : (it.kind === 'svc' ? 'cpu' : 'box');
        return '<label class="su-optrow">'
          + '<input type="checkbox" data-su-opt="' + suEsc(it.id) + '" checked>'
          + '<div class="su-ico" data-su-ico="' + suEsc(it.iconKey || '') + '">' + ico(kindIco, 16) + '</div>'
          + '<div class="su-main">'
          +   '<div class="su-nm"><span class="su-nmt">' + suEsc(it.name) + '</span><i class="su-tag kind">' + (SU_KIND[it.kind] || it.kind) + '</i></div>'
          +   '<div class="su-sub" title="' + suEsc(it.why || '') + '">' + suEsc(it.desc || it.pub || '无描述信息') + (it.why ? ' · ' + suEsc(it.why) : '') + '</div>'
          + '</div></label>';
      }).join('')
    + '</div>'
    + '<div class="su-optbar"><span id="suOptCnt"></span>'
    + '<div class="su-optbtns"><button class="btn" data-act="c">取消</button>'
    + '<button class="btn primary" id="suOptGo">' + ico('bolt', 14) + '立即优化</button></div></div>');
  var upd = function(){
    var el = suEl('suOptCnt');
    if(el) el.textContent = '已选 ' + document.querySelectorAll('#suOptList [data-su-opt]:checked').length + ' / ' + adv.length + ' 项';
  };
  suEach('#suOptList [data-su-opt]', function(c){ c.onchange = upd; });
  upd();
  suModalIcons();
  var c = suEl('modalBox').querySelector('[data-act="c"]');
  if(c) c.onclick = suModalClose;
  var g = suEl('suOptGo');
  if(g) g.onclick = suOptRun;
}
function suOptRun(){
  var ids = [];
  suEach('#suOptList [data-su-opt]:checked', function(c){ ids.push(c.getAttribute('data-su-opt')); });
  if(!ids.length){ toast('请先勾选要优化的项目', 'danger'); return; }
  var done = 0, fail = 0;
  var prog = function(){
    return '<div class="modal-t">' + ico('bolt', 18) + '正在优化…</div>'
      + '<div class="modal-d">正在逐项禁用，请不要关闭窗口</div>'
      + '<div class="su-prog"><div class="su-prog-b" style="width:' + Math.round(done / ids.length * 100) + '%"></div></div>'
      + '<div class="su-prog-t">' + done + ' / ' + ids.length + '</div>';
  };
  suModal(prog());
  (function step(){
    if(done >= ids.length){
      suSyncGlobal();
      suModal('<div class="su-done"><div class="su-done-ico">' + ico('check', 28) + '</div>'
        + '<h3>优化完成</h3>'
        + '<p>' + (ids.length - fail) + ' 项已禁用' + (fail ? ('，' + fail + ' 项处理失败') : '') + '。<br>下次开机明显提速；反悔了随时在「优化记录」里恢复。</p>'
        + '<button class="btn primary" data-act="c">好的</button></div>');
      var c = suEl('modalBox').querySelector('[data-act="c"]');
      if(c) c.onclick = suModalClose;
      suForceLoad();
      return;
    }
    var id = ids[done];
    callGo('SetStartupEnable', id, false).then(function(){
      SU_S.items.forEach(function(x){ if(x.id === id) x.enabled = false; });
    }, function(){ fail++; }).then(function(){
      done++;
      suModal(prog());
      step();
    });
  })();
}
/* 弹窗里的图标：直接复用列表那套队列（按 iconKey 去重 + 内存缓存）。
   一键优化弹窗里就是列表里那批项，缓存命中就同步贴上，命不中也只会问一次后端。 */
function suModalIcons(){
  suIcons(suEl('modalBox'));
}

/* ── 角标计数 ───────────────────────────────────────────────────────────── */
function suBadges(){
  callGo('GetStartupIgnored').then(function(ig){
    var el = suEl('suIgnN'); if(el) el.textContent = (ig || []).length;
  }).catch(function(){});
  callGo('GetStartupLog').then(function(lg){
    var el = suEl('suLogN'); if(el) el.textContent = (lg || []).length;
  }).catch(function(){});
}

/* ── 事件委托（挂 #content，只绑一次；重绘只换 innerHTML，元素不重建）───── */
function suClick(e){
  var t = e.target, el;
  if(!t || !t.closest) return;
  if((el = t.closest('#suOptBtn'))){ suOptModal(); return; }
  if((el = t.closest('#suReload'))){ suForceLoad(); return; }
  if((el = t.closest('#suLogBtn'))){ suLogModal(); return; }
  if((el = t.closest('#suIgnBtn'))){ suIgnoreModal(); return; }
  if((el = t.closest('[data-su-tab]'))){
    SU_S.tab = el.getAttribute('data-su-tab');
    suRenderAll();
    return;
  }
  if((el = t.closest('[data-su-sort]'))){
    SU_S.sort = el.getAttribute('data-su-sort');
    suRenderList();
    return;
  }
  if((el = t.closest('[data-su-more]'))){ e.stopPropagation(); suMenu(el); return; }
}
function suChange(e){
  var t = e.target;
  if(!t) return;
  if(t.id === 'suHideChk'){
    SU_S.hide = !!t.checked;
    suRenderList();
    return;
  }
  if(t.id === 'suSysChk'){ suToggleSys(t.checked); return; }
  var id = t.getAttribute && t.getAttribute('data-su-toggle');
  if(id) suToggle(t, id, t.checked);
}
function bindStartup(){
  var root = document.getElementById('content');
  if(!root) return;
  if(root.getAttribute('data-suBound') === '1'){ suAfterPaint(); return; }
  root.setAttribute('data-suBound', '1');
  root.addEventListener('click', suClick);
  root.addEventListener('change', suChange);
  suAfterPaint();
}
/* 每次整页重绘后都要补画（列表在 #suList 里，重绘后是空的） */
function suAfterPaint(){
  /* 角标（优化记录 / 忽略区）跟列表数据无关，任何分支下都得显示真实条数。
     放在两个提前 return 之后过 —— 进页正好赶上后台还在扫时，角标会一直停在 0。 */
  suBadges();
  /* 还在扫时只有"一条数据都没有"才铺骨架，有旧数据就留着旧的（别闪一下空白） */
  if(SU_S.loading){ if(!SU_S.items.length) suPaintLoading(); return; }
  if(!SU_S.loaded){ suLoad(); return; }
  suRenderAll();
}

/* ⚠️ 必须挂 window：加载器是 IIFE 里 eval，主文件的 PAGES 表要引用 renderStartup */
window.renderStartup = renderStartup;
window.bindStartup   = bindStartup;
window.suLoad        = suLoad;
window.suForceLoad   = suForceLoad;
window.SU_S          = SU_S;
