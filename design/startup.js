/* ══════════════════════════════════════════════════════════════════════════
   启动项管理 · 独立模块（design/startup.js）
   ---------------------------------------------------------------------------
   功能与交互 100% 照老版本（v2 启动项管理 2.0）搬过来：
     · 顶部开机耗时条（切线对比：这次开机多久 / 关掉能到多少，见 suBpHtml）
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
var SU_S = {loaded:false, loading:false, tab:'run', items:[], hide:false, showSys:false, sort:'advice', err:'', diag:{}, admin:true, boot:null};
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
   它和「建议禁用」是两个动作：禁用可恢复，删除不可恢复，所以不进一键优化清单。 */
function suDel(){
  return SU_S.items.filter(function(i){ return i.advice === 'delete'; });
}
/* 排序权重：建议删除 > 建议禁用 > 保持现状（和 Go 侧 adviceRank 一致） */
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
  /* 2026-10-26 晚：顶部从「引导卡 + 开机耗时 + 影响项」三块砍到**一块**。
     ① 引导卡（管什么 / 关掉后 / 改错了）是给原来挂在同一行右边的一键优化按钮
        打底的；按钮挪走后它就和按钮下方的说明重复，「关掉后开机更快」并进副标题。
     ② 开机耗时和影响项本来就是同一件事的三段（多久 · 谁拖的 · 要不要处理），
        拆两张卡纯属浪费首屏：两张叠起来近 300px，第一屏只能看到工具条。
        现在合成一张 #suPerf，见 suRenderPerf()。 */
  /* 页头也压成一行（.su-head）：标题 + 一句说明同基线，不再占两行 78px。
     副标题（2026-10-27 定稿）：先说收益「提升开机速度」，再说安心「随时恢复」。
     原来的「只管是否自动运行」是解释软件做了什么，用户关心的是他能得到什么。 */
  return '<div class="page-head su-head"><div class="page-title">启动项管理</div>'
    + '<div class="page-sub" title="关掉平时用不到的启动项和服务项，它们不再开机自动运行，开机就能少等一会儿；'
    + '功能一点不少，随时拨回来就能恢复">关掉无用的启动项和服务项，可以提升电脑开机速度！可随时恢复！</div></div>'
    + '<div id="suPerf"></div>'
    + '<div id="suTip"></div>'
    + '<div class="card su-bar">'
    +   '<div class="su-seg" id="suTabs"></div>'
    +   '<div class="su-bar-act">'
    +     '<button class="btn sm" id="suReload" title="重新读取全部启动项（含最新安装 / 卸载的变化）">' + ico('refresh', 14) + '<span>重新扫描</span></button>'
    +     '<button class="btn sm" id="suLogBtn" title="查看最近的启用 / 禁用操作，可一键恢复">' + ico('doc', 14) + '<span>优化记录</span> <b id="suLogN">0</b></button>'
    +     '<button class="btn sm" id="suIgnBtn" title="被忽略、不再出现在列表里的启动项">' + ico('fold', 14) + '<span>忽略区</span> <b id="suIgnN">0</b></button>'
    /* ⚠️ checked 要按内存里的 SU_S.hide 写死。页面重绘（切走再回来）会重建这段 HTML，
       不写的话勾选框回到未勾，而 suRows() 仍按 hide=true 过滤 —— 列表少了一半行，
       却没有任何筛选标记，用户只会以为"我的启动项丢了"。
       「显示系统自带」同理，那个靠 suSyncSysChk 从后端回读，见 suRenderAll。 */
    +     '<label class="btn sm" title="只看当前还在运行的启动项"><input type="checkbox" id="suHideChk"'
    +       (SU_S.hide ? ' checked' : '') + '><span>隐藏已禁用</span></label>'
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
/* 重扫进行中：按钮转圈并禁用，防连点。
   ⚠️ 状态必须存在变量里，不能只写 DOM：这一条 strip 会被 suRenderPerf 整块或
     局部重画，新按钮天生是 enabled 的。suForceLoad 的顺序是「先 suScanBusy(true)
     → 再 suReloadBoot()」，而 suReloadBoot 回来就会重画一次 —— 只写 DOM 的话，
     转圈刚亮就被新按钮抹掉，用户看到按钮能点，点下去又是一次扫描。 */
var SU_SCAN_BUSY = false;
function suScanBusy(on){
  SU_SCAN_BUSY = !!on;
  /* 清理无效项也一起禁掉：删除是不可逆动作，而扫描期间列表还是上一批数据 ——
     刚在别处被恢复的项如果还留在这批里，用户一勾就把它彻底删了。 */
  suEach('#suReload, #suOptBtn, #suDelBtn', function(b){
    b.disabled = !!on; b.classList.toggle('busy', !!on);
  });
}

function suLoad(){
  if(SU_S.loading) return;            /* 防并发重复扫描（连点刷新 / 弹窗内恢复后） */
  SU_S.loading = true; SU_S.loaded = false;
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
  /* 开机耗时：读不到（非管理员 / 日志没启用）就整块不渲染，绝不拦住页面 */
  callGo('GetBootPerf').then(function(b){
    var oldTag = JSON.stringify(SU_BOOT_TAG);
    SU_S.boot = b || null;
    if(typeof cur !== 'undefined' && cur === 'sec'){
      suRenderPerf();
      /* 列表里的「拖慢 X 秒」标签依赖 SU_BOOT_TAG，而它是在 suBpCalc 里
         填的 —— 开机数据比 ListStartup 回来得晚，这时列表已经画过一遍了。
         不补这一次重绘，标签要等用户切页回来才出现。
         ⚠️ 只在标签表真的变了才重画：进页时列表本来就被画过 2~3 次
         （suRenderAll / 诊断回来 / 这里），无条件再拆一次整表，几十行 DOM
         白建一遍，图标队列也要多走一轮 —— 观感就是"进页抖一下"。 */
      if(JSON.stringify(SU_BOOT_TAG) !== oldTag) suRenderList(true);
    }
  }).catch(function(){
    var oldTag = JSON.stringify(SU_BOOT_TAG);
    SU_S.boot = null;
    if(typeof cur !== 'undefined' && cur === 'sec'){
      suRenderPerf();
      if(JSON.stringify(SU_BOOT_TAG) !== oldTag) suRenderList(true);
    }
  });
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

/* ── 开机耗时卡（实测展示）──────────────────────────────────────────────
   ⚠️ 刻意只报实测，不报"优化后能快几秒"：
      实测多项启动项耗时相加会超过 PostBoot 实际总时长（并行重叠），
      相加模型在数学上就不成立；预测几秒必然被打脸。
      所以这里给的是"现在多快"和"谁在拖后腿"，不给任何承诺值。 */
function suSec(ms){
  ms = Number(ms) || 0;
  return ms > 0 ? (ms / 1000).toFixed(1) : '--';
}

/* ── 动效：把「读到了什么」演出来，而不是直接摊在屏幕上 ─────────────────────
   这一条要表达的是"开机花了多久" —— 那本身就是一段时间。数字从 0 滚上去，
   比一个静止的 15.4 更接近它要说的事；滚完"咔"一下定住，表示读数到位。
   另外三块（后台、点名、按钮）按阅读顺序错峰浮现，眼睛有落点，不会一瞬间
   全砸出来。纯 CSS 管错峰，这里只管数字。
   ⚠️ 一律尊重 html[data-motion="off"]（其他页都是这个开关），关掉就直接给结果。
*/
var SU_CU_SIG = '';   /* 上次滚的是哪批数据：切分类/重扫/批量禁用都会重画整条，不该每次都从头数 */

function suCountUp(dur){
  if(document.documentElement.getAttribute('data-motion') === 'off') return;
  var D = dur || 1000;   /* 重播时和条形一样快，否则条都长完了数字还在爬 */
  suEach('[data-cu]', function(el){
    var to = Number(el.getAttribute('data-cu')) || 0;
    if(to <= 0) return;
    var txt = suSec(to);
    /* 先让浏览器把最终值画出来，下一帧才归零开滚 —— 直接写 0.0 的话，
       万一 requestAnimationFrame 不跑（省电模式/后台标签页），那个假的 0.0 就留下了。 */
    requestAnimationFrame(function(){
      el.textContent = '0.0';
      var t0 = 0;
      requestAnimationFrame(function step(ts){
        if(!t0) t0 = ts;
        var k = Math.min(1, (ts - t0) / D);
        var e = 1 - Math.pow(1 - k, 3);        /* ease-out cubic：先快后慢，像真的在收尾 */
        el.textContent = (to / 1000 * e).toFixed(1);
        if(k < 1) return requestAnimationFrame(step);
        el.textContent = txt;
        /* 定住时轻弹一下，读作"读数到位"。动画关掉时这段不会执行到。 */
        var box = el.parentNode;
        if(box && box.classList){
          box.classList.remove('done');
          void box.offsetWidth;                /* 强制回流，否则连点两次不重放 */
          box.classList.add('done');
        }
      });
    });
  });
}
/* 读不到开机数据时的引导。
   ⚠️ 这里以前是"整块留空"，在开发者机器上永远看不到 —— 因为本机永远读得到。
   换台机器（非管理员 / 日志被别的优化软件关了）就会变成一片空白，用户只会
   以为功能坏了。所以按后端给的原因码分别说明，能修的给按钮。
   errCode 是后端算好的，前端不许拿 err 文本做匹配（文本会改）。 */
var SU_BOOT_FIX = {
  noadmin:  {ico:'shield', txt:'需要管理员权限才能读取 Windows 开机记录', btn:''},
  disabled: {ico:'clock',  txt:'Windows 开机性能日志已关闭，开启后需重启一次才会开始记录', btn:'开启'},
  nolog:    {ico:'info',   txt:'当前系统没有开机性能日志，此功能不可用', btn:''},
  empty:    {ico:'clock',  txt:'还没有开机记录，正常开关机几次后会自动出现', btn:'重新检测'},
  timeout:  {ico:'clock',  txt:'读取超时，可稍后重试', btn:'重试'},
  notool:   {ico:'info',   txt:'系统缺少 wevtutil 组件，无法读取开机记录', btn:''},
  parse:    {ico:'info',   txt:'开机记录解析失败', btn:'重试'}
};

/* 强制重读开机数据（绕过后端 60 秒缓存）。批量禁用/清理之后也走这里，
   让卡片立刻反映最新状态，而不是等缓存过期。
   ⚠️ 2026-10-26 踩过的坑：GetBootPerf 比 ListStartup 回来得晚（要查事件日志），
   而一键优化清单里混着「Windows 实测点名的项」。以前拆成两张卡、只重画上面那张，
   就出现「上面写本次最慢 42.5 秒，下面写暂无实测耗时数据，一键优化少算一项」。
   现在合成一张 suRenderPerf()，任何一处数据变化都整块重画，天然不会再分叉。 */
function suReloadBoot(){
  callGo('RefreshBootPerf').then(function(b){
    SU_S.boot = b || null;
    suRenderPerf();
  }).catch(function(){ suRenderPerf(); });
}

/* 提示条上的动作：disabled 去开日志，其余强制重读。 */
function suBootFix(code){
  if(code === 'disabled'){
    callGo('EnableBootLog').then(function(ok){
      if(ok) toast('已开启开机性能日志，重启一次后开始记录', 'ok');
      else toast('开启失败，需要管理员权限', 'warn');
      suReloadBoot();
    }).catch(function(){ toast('开启失败，需要管理员权限', 'warn'); });
    return;
  }
  suReloadBoot();
}

/* 强制重扫：绕过后端 10 分钟缓存。
   重扫本身要花几百毫秒（几百条服务 / 任务要重新读），先让按钮转圈，
   否则从点到按钮有反应之间有一段"什么都没发生"的空窗。 */
function suForceLoad(){
  suScanBusy(true);
  /* 重扫启动项的同时也要刷开机数据：批量禁用/清理之后，状态条里的点名项
     和按钮计数都变了，后端那份 60 秒缓存必须一起失效，
     否则会出现"刚禁用了它，条上还写着可以禁用"的自相矛盾。 */
  suReloadBoot();
  callGo('ListStartupForce').then(function(){ suLoad(); },
    function(){ suLoad(); });
}

/* ── 实测点名（Windows 事件日志）× 启动项列表 ────────────────────────────── */

/* Windows 实测点名、我们管得到、且当前还启用着的项 —— 这才配叫「影响开机速度」。
   两类要滤掉，否则会给出点了没用的按钮：
     · itemId 为空    = 不在启动项列表里（svchost、浏览器会话恢复、别的软件的子进程）
     · itemEnabled=假 = 已经禁用了还在拖慢，说明它另有启动途径，禁用按钮毫无意义 */
/* 本次开机被 Windows 点名、并且能在启动项列表里找到的那些项。
   ⚠️ 只看 records[0]，不遍历历史。老版本取"每项历史最大耗时"，会把不同次
      开机的数字并排列出来（豆包 49.2 秒那次是 10-26，86.2 秒是 10-27），
      用户一重启就发现对不上 —— 这是这一版必须修掉的口径错误。
   ⚠️ itemId 为空 = 事件点名了但启动项列表里没有（svchost、浏览器会话恢复、
      别的软件的子进程），既给不了按钮，也不该算进"启动项占用的时间"。
   ⚠️ 不过滤 itemEnabled：已关掉的项要留着，否则算不出"用户已经省了多少"。 */
function suBootHits(){
  var b = SU_S.boot;
  if(!b || !b.ok) return [];
  var rec = (b.records || [])[0];
  if(!rec || !rec.slowest) return [];
  var out = [];
  rec.slowest.forEach(function(c){
    if(!c.itemId) return;
    out.push({ c: c, start: rec.bootStart });
  });
  return out.sort(function(x, y){ return y.c.totalMs - x.c.totalMs; });
}
function suById(id){
  for(var i = 0; i < SU_S.items.length; i++){
    if(SU_S.items[i].id === id) return SU_S.items[i];
  }
  return null;
}
function suDay(s){
  try{
    var d = new Date(s);
    if(isNaN(d.getTime())) return '';
    return (d.getMonth() + 1) + '-' + ('0' + d.getDate()).slice(-2);
  }catch(e){ return ''; }
}

/* 一键优化清单 = 实测点名的项 + 建议禁用的项（按 id 去重，实测项排前面）。
   ⚠️ 2026-10-27 改：实测项从"默认不勾"改成**默认勾上**。
      原来的理由是把决定权留给用户，但代价是普通用户点完"一键优化 3 项"，
      只关掉两个 Office 遥测，开机时间几乎没变 —— 软件等于没帮上忙。
      现在默认勾上、弹窗里逐项列秒数、想留的随手取消：决定权还在他手上，
      只是默认值从"什么都不做"换成"按证据做"。
   ⚠️ ms 用 degradMs 不是 totalMs：弹窗里写「拖慢 X 秒」，就必须是
      "因为它，开机多花了多久"，否则和顶部那条切线减掉的数对不上。 */
function suPlanList(){
  var out = [], got = {};
  suBootHits().forEach(function(h){
    var it = suById(h.c.itemId);
    if(!it || !it.enabled) return;
    got[it.id] = 1;
    out.push({it: it, ms: Math.max(0, Number(h.c.degradMs) || 0),
      day: suDay(h.start), hot: true, on: true});
  });
  suAdv().forEach(function(it){
    if(got[it.id]) return;
    out.push({it: it, ms: 0, day: '', hot: false, on: true});
  });
  return out;
}

/* ── 开机耗时归因条（2026-10-27 第十二版：切线对比）──────────────────────
   第十一版把总时长切成"灰（Windows）+ 橙（启动项）"两段，用户反馈：
   "看不懂""脱离设计的初衷"。问题不在数据，在叙事 —— 构成条回答的是
   "时间花在哪"，用户问的是"关掉之后能快多少"。两个问题，一颗子弹打不完。
   这一版改成一次**剖切**：
     这次开机 148 秒 · 31 项自启        关掉这 8 项 → 96 秒
     [ Windows 62 秒 ][ 启动项 86 秒 |▓▓ 省 52 秒 ▓▓ ]
                                     ↑ 白线 = 切在这里
   白线右边压暗的那段直接写"省 52 秒"，不用读句子：条被切掉一截就是答案，
   切口位置 = 关掉之后的预计时长。没得切的时候不画白线，右边改说
   "没有可省的启动项" —— 切下来只剩一条缝，那不叫提升，叫装样子。

   ⚠️ 口径一律用本次开机（records[0]）。早期版本遍历全部历史取"每项最大耗时"，
      会把 10-26 那次豆包的 49.2 秒和 10-27 的 86.2 秒并排列出来。两个数不是
      同一次开机，用户重启一次就发现对不上。
   ⚠️「省多少」只用 degradMs，不用 totalMs。totalMs 是"它自己跑了多久"，
      degradMs 才是"因为它，开机多花了多久"。本机实例：向日葵 total 8.1 秒，
      degrad 只有 2.1 秒，差 4 倍。拿 totalMs 当省下的量就是在夸大。
   ⚠️ 关掉某项后，省下的 degradMs 从总数扣除，而该项剩下的
      totalMs - degradMs 并入灰段 —— 时间不会凭空消失，只是被别的后台程序占了。
      不这么算，灰段 + 橙段 ≠ 总数。
*/
var SU_BOOT_TAG = {};   /* itemId → 该项自身耗时(ms)，列表排序用 */

/* 把本次开机拆成 系统 / 启动项 / 能省 三份。返回 null 表示没有可用数据。
   ⚠️ 一切以本次开机为准（records[0]）。历史记录只用来"打招呼"（见 riskList），
      绝不参与这里的任何数字 —— 第十版的教训是：把不同次开机的秒数并排写出来，
      用户重启一次就发现对不上。 */
function suBpCalc(){
  var b = SU_S.boot;
  if(!b || !b.ok || !b.records || !b.records.length) return null;
  var rec = b.records[0];
  var mainMs = Math.max(0, Number(rec.mainPathMs) || 0);
  var postMs = Math.max(0, Number(rec.postBootMs) || 0);
  var totalMs = mainMs + postMs;
  if(totalMs <= 0) return null;

  var hits = suBootHits();
  var appMs = 0, canSave = 0, nSave = 0, nOff = 0, tag = {}, named = {};
  hits.forEach(function(h){
    var it = suById(h.c.itemId);
    /* 用启动项列表的当前状态，不用事件里那一刻的 itemEnabled ——
       用户刚拨完开关时，后端还没重新扫描，事件里的值仍是旧的。 */
    var on = it ? it.enabled : h.c.itemEnabled;
    var dm = Math.max(0, Number(h.c.degradMs) || 0);
    var tm = Math.max(0, Number(h.c.totalMs) || 0);
    /* 列表标签和排序都用 degradMs，不是 totalMs：
       "它跑了 3.8 秒"和"它让你多等了 1.2 秒"是两件事。标签写「拖慢」，
       就必须用后者，否则用户会拿标签的数和顶部减掉的数对不上。 */
    tag[h.c.itemId] = dm;
    named[h.c.itemId] = 1;
    /* ⚠️ 能不能"省"必须先确认这一项在列表里、而且拨得动（it 存在）。
       it 为 null 有两种：这一项被用户忽略掉了，或者它是系统自带项没勾「显示系统自带」。
       这两种情况下「一键优化」根本列不出它 —— 若这里仍把 dm 累进 canSave，
       顶部就会承诺一个软件做不到的秒数，和右边按钮的项数对不上。
       appMs（橙段长度）仍然算它：它确实在这台机器的开机里跑了，条不能说谎。 */
    if(on){ appMs += tm; if(it){ canSave += dm; nSave++; } }
    else  { if(it) nOff++; }
  });
  SU_BOOT_TAG = tag;

  /* ── 历史前科：只打一句招呼，不进任何数字、也不画任何形状（2026-10-27）
     用户的抱怨："我把之前禁用的项打开，橙色条没有变化！"
     根因不是 bug：Windows 只点名本次开机最慢的 3 项，用户打开的钉钉那次开机
     还没启用，日志里根本没有它 —— 本次数据不可能变。
     上一版画了一条虚线段表示"还没发生"，用户反馈"看不懂，脱离初衷"。
     这一版只留一句话：这一项在以前某次开机被点名过，那次实测拖了 N 秒。
     数字不进条、不进总数 —— 条的长度必须永远是这一次开机的实测值。
     ⚠️ 1 秒以下不收：Windows 会把 0.1~0.5 秒的项也列进 101 事件，
        拿它们去说事，只会让人觉得软件在小題大做。 */
  var riskMap = {};
  var recs = b.records || [];
  for(var i = 1; i < recs.length; i++){
    var rr = recs[i];
    (rr.slowest || []).forEach(function(c){
      if(!c.itemId || named[c.itemId]) return;
      var dm2 = Math.max(0, Number(c.degradMs) || 0);
      if(dm2 < 1000) return;
      var old = riskMap[c.itemId];
      if(!old || dm2 > old.dm) riskMap[c.itemId] = { dm: dm2, start: rr.bootStart };
    });
  }
  var riskList = [], riskMs = 0;
  (SU_S.items || []).forEach(function(it){
    if(!it.enabled || named[it.id]) return;
    var rk = riskMap[it.id];
    if(rk){ riskMs += rk.dm; riskList.push({ name: it.name, dm: rk.dm, start: rk.start }); }
  });
  riskList.sort(function(x, y){ return y.dm - x.dm; });

  /* 自启项数只数 kind==='run'：页签里「启动项」才是这块讲的对象，
     服务项和计划任务不参与（Windows 的 BootNumStartupApps 也只算 Run 项）。
     把服务和任务算进来会变成三四十，"31 项自启"就没人信了。 */
  var nRun = 0;
  (SU_S.items || []).forEach(function(it){ if(it.kind === 'run' && it.enabled) nRun++; });

  var sysMs = Math.max(0, totalMs - appMs);
  var willMs = Math.max(0, totalMs - canSave);
  /* 切线只在"真的切得下东西"时画：不到 1 秒、或不到总时长 5%，
     切下来就是一条缝，那不叫提升，叫装样子。
     ⚠️ canSave 恒 ≤ appMs（每项 degrad ≤ total），所以切口一定落在橙段里，
        不会出现"切掉一块 Windows"的荒唐画面。 */
  var showCut = canSave >= 1000 && canSave / totalMs >= 0.05;

  return {
    totalMs: totalMs, sysMs: sysMs, appMs: appMs,
    canSave: canSave, willMs: willMs, nSave: nSave, nOff: nOff,
    nRun: nRun, riskList: riskList, riskMs: riskMs, showCut: showCut,
    recCount: recs.length
  };
}

/* 第一行右侧那句话。四种状态各说一句人话，不堆术语。
   优先级：有能关的 > 打开了有前科的项 > 已经关过 > 都没有。
   为什么"能关"排最前：它是这块唯一能兑现的承诺，也是用户扫一眼就想知道的。
   为什么"前科"排第二：它不是数字（本次没数据），只能用一句话提个醒；
   但如果它排在"能关"前面，用户会以为这次也能省，那就成了误导。 */
function suBpRight(k){
  if(!k) return '';
  /* canSave>0 才算"能省"：suSec(0) 会输出 "--"，而"关掉 1 项可省 -- 秒"
     看起来像数据坏了。被点名但 degradMs 为 0（跑了但没让系统多等）是真的会发生的。 */
  if(k.nSave > 0 && k.canSave > 0){
    /* 省得够多（够画切线）才给"→ 终点"：那时条上有白线做锚点，数字可信。
       省不到 5% 时只报"可省多少"，不给终点 —— 86.2 和 85.0 并排摆着，
       视觉上等于没差，反而像在凑数。 */
    return k.showCut
      ? '关掉 ' + k.nSave + ' 项 → <b class="ok">' + suSec(k.willMs) + '</b> 秒'
      : '关掉 ' + k.nSave + ' 项可省 <b class="ok">' + suSec(k.canSave) + '</b> 秒';
  }
  if(k.riskList.length){
    /* 写「上次开机」不写「刚打开」：程序分不清这一项是你刚拨开的、还是一直开着。
       说了"刚打开"，用户会以为软件在指责他，而他可能根本没碰过那一项。 */
    var r0 = k.riskList[0];
    var who = k.riskList.length > 1
      ? suEsc(r0.name) + ' 等 ' + k.riskList.length + ' 项'
      : suEsc(r0.name);
    return '<b class="warn">' + who + '</b> 上次开机拖了 ' + suSec(k.riskMs) + ' 秒';
  }
  /* 没有实测秒数时，至少要和「一键优化 N」按钮对上 —— 否则上面说"没有可省的"、
     右边按钮却写着"优化 2 项"，用户会以为有一边在骗人。
     这两类项的区别是：它们没被 Windows 点名过，关掉也量不出提速，但仍然该关。
     ⚠️ 用「另有」不用「还有」：左边那个"N 项自启"只数 Run 项，
        而建议关闭的常常是计划任务/服务，两个数字口径不同，硬凑成一对会被追问
        "0 项自启哪来的 2 项可关"。 */
  /* 被点名、但 degradMs 为 0：它确实在开机时跑了，却没让系统多等。
     属于"没什么可省的"，但不能写成"关掉可省 -- 秒"。 */
  if(k.nSave > 0) return '这 ' + k.nSave + ' 项测不出明显拖慢';
  var nAdv = suAdv().length;
  if(nAdv > 0) return '另有 ' + nAdv + ' 项待优化';
  if(k.nOff > 0) return '已关掉 ' + k.nOff + ' 项，没有其他可省';
  return '没有可省的启动项';
}

/* 右侧那句话的 tooltip。抽成函数是因为它必须跟着状态变：刚打开一个有前科的项时
   说"上次拖了多少"，关掉几项之后说"已关掉、等下次开机看效果"。两处（首次渲染 /
   拨开关原地更新）共用一份文案，避免只改一处、另一处写着上一秒的事。 */
function suBpRightTip(k){
  if(!k) return '';
  if(k.nSave > 0 && k.canSave > 0){
    return '这 ' + k.nSave + ' 项是本次开机被 Windows 实测点名、且当前还开着的。'
      + '禁用只是不再开机自启，软件照常能用，随时能拨回来。';
  }
  if(k.riskList.length){
    return '它们本次开机没被点名（Windows 每次只记最慢的 3 项），这是上一次被点名时的实测值。'
      + '重启一次才会有新数据。';
  }
  return '只有被 Windows 点名过的项才有实测秒数；没被点名的项本来就不慢。';
}

function suBpHtml(k){
  if(!k) return '';
  var den = k.totalMs > 0 ? k.totalMs : 1;
  var pct = function(v){ return (v / den * 100).toFixed(2) + '%'; };
  var sysPct = k.sysMs / den * 100;
  var appPct = k.appMs / den * 100;
  var cutPct = k.willMs / den * 100;
  var savePct = 100 - cutPct;

  var tip = '从 Windows 内核加载起算，不含 BIOS/POST 自检；取最近 '
    + k.recCount + ' 次开机里的最近一次。';
  /* 「只有你能改」这句话塞进数字的 title：鼠标悬停能看到，不占版面。 */
  var tipNum = '这台电脑的开机时间只有两部分：Windows 自身启动（所有电脑都有，改不了）'
    + '+ 启动项（只有你能改）。' + tip;

  /* 段内写字的三档阈值按实测像素定：条满宽约 568px。
     sys 需要放下"Windows 自身 76.8 秒"（约 130px，23%），app 放下"启动项 9.4 秒"
     （约 90px，16%），压暗段放下"省 21.3 秒"（约 66px，12%）。
     不到就只留色块 —— 文字被裁成半个数字比不写更糟。 */
  var sysTxt = sysPct >= 24 ? 'Windows 自身 ' + suSec(k.sysMs) + ' 秒' : '';
  var appTxt = appPct >= 13 ? '启动项 ' + suSec(k.appMs) + ' 秒' : '';
  var cutTxt = k.showCut && savePct >= 12 ? '省 ' + suSec(k.canSave) + ' 秒' : '';

  var right = suBpRight(k);
  var rightTip = suBpRightTip(k);

  /* 橙段里的字必须避开刀口：padding-right 空出被切掉的那一截（百分比按条的
     总宽算，正好等于压暗段的宽度），文字于是居中在"剩下的那半截"里。
     不这么做的话，"启动项 49.2 秒"会被压暗层盖掉一半，读起来像数据坏了。 */
  var appStyle = 'width:' + pct(k.appMs);
  if(k.showCut && cutPct > sysPct){
    appStyle += ';padding-right:' + (100 - cutPct).toFixed(2) + '%';
  }

  return '<div class="su-bp">'
    + '<div class="su-bp-top" title="' + suEsc(tip) + '">'
      + '<span class="su-bp-l">这次开机</span>'
      + '<b class="su-bp-n" title="' + suEsc(tipNum) + '"'
      + ' data-cu="' + Math.round(k.totalMs) + '">' + suSec(k.totalMs) + '</b>'
      + '<i>秒</i>'
      + '<span class="su-bp-cnt">· ' + k.nRun + ' 项自启</span>'
      + '<span class="su-bp-s" title="' + suEsc(rightTip) + '">' + right + '</span>'
    + '</div>'
    + '<div class="su-bp-brow">'
      + '<div class="su-bp-bar">'
        + (k.sysMs > 0
            ? '<span class="su-bp-sys" style="width:' + pct(k.sysMs) + '">' + sysTxt + '</span>' : '')
        + (k.appMs > 0
            ? '<span class="su-bp-app" style="' + appStyle + '" title="启动项占用 '
              + suSec(k.appMs) + ' 秒：本次开机被 Windows 点名、且咱们管得到的项">'
              + appTxt + '</span>' : '')
        + (k.showCut
            ? '<span class="su-bp-cutbg" style="left:' + cutPct.toFixed(2) + '%"></span>'
              + '<span class="su-bp-ct" style="left:' + cutPct.toFixed(2) + '%"></span>'
              + '<span class="su-bp-ctxt" style="left:' + cutPct.toFixed(2) + '%">'
              + cutTxt + '</span>'
            : '')
      + '</div>'
    + '</div>'
    + '</div>';
}

/* 数字从 fromMs 滚到 toMs。和 suCountUp 的区别：那个是从 0 滚（首次绘制），
   这个是从当前读数滚（拨开关后），看着才像"数字跟着我的操作变了"。 */
function suRollNum(el, fromMs, toMs, dur){
  if(document.documentElement.getAttribute('data-motion') === 'off'){
    el.textContent = suSec(toMs); return;
  }
  var D = dur || 560, t0 = 0;
  requestAnimationFrame(function step(ts){
    if(!t0) t0 = ts;
    var p = Math.min(1, (ts - t0) / D);
    var e = 1 - Math.pow(1 - p, 3);
    el.textContent = suSec(fromMs + (toMs - fromMs) * e);
    if(p < 1) return requestAnimationFrame(step);
    el.textContent = suSec(toMs);
  });
}

/* 原地更新（拨开关时用）。不重建 DOM：重建会让 .su-strip 重播 suStripIn，
   那正是用户说的"整页闪一下"。这里只改数字、两段宽度和切线位置，CSS 的
   transition 自己会把条收窄、把切口挪过去、把数字滚下去。 */
function suBpUpdate(k){
  var host = document.querySelector('.su-bp');
  if(!host || !k) return;
  var n = host.querySelector('.su-bp-n');
  var s = host.querySelector('.su-bp-s');
  var cnt = host.querySelector('.su-bp-cnt');
  var sys = host.querySelector('.su-bp-sys');
  var app = host.querySelector('.su-bp-app');
  var cutbg = host.querySelector('.su-bp-cutbg');
  var ct = host.querySelector('.su-bp-ct');
  var ctxt = host.querySelector('.su-bp-ctxt');

  var den = k.totalMs > 0 ? k.totalMs : 1;
  var cutPct = k.willMs / den * 100;
  var savePct = 100 - cutPct;

  /* 结构对不上就整块重画。三种"从无到有"都会让后面的赋值打在 null 上：
       ① 橙段：全关掉时 appMs=0 不渲染，拨回启用后得造出来；
       ② 切线：省得够多才出现（或关掉一项后掉到 5% 以下要收回去）；
       ③ 灰段：理论上恒有，但 mainPathMs 异常时可能为 0。
     这种"从无到有"本来也没有过渡可言，重建不亏（重建的是 .su-bp 内部，
     不是 .su-strip，所以不会重播整卡的入场动画）。 */
  var needApp = k.appMs > 0 && !app;
  var needCut = k.showCut && !cutbg;
  var dropCut = !k.showCut && cutbg;
  var needSys = k.sysMs > 0 && !sys;
  if(needApp || needCut || dropCut || needSys){
    var wrap = host.parentNode;
    if(wrap){
      wrap.innerHTML = suBpHtml(k);
      /* 重建出来的是新元素，clip-path 生长动画会从头再播一遍 —— 拨个开关
         让整条重新"长"一次，就是用户之前投诉的"闪一下"。首屏该播的已经播过
         了，这里直接关掉；宽度也没有过渡可言（新节点，没有起始值）。 */
      var bar = wrap.querySelector('.su-bp-bar');
      if(bar) bar.style.animation = 'none';
      return;
    }
  }

  if(n){
    var from = Number(n.getAttribute('data-cu')) || 0;
    var to = Math.round(k.totalMs);
    n.setAttribute('data-cu', to);
    suRollNum(n, from, to, 560);
  }
  if(s){
    s.innerHTML = suBpRight(k);
    s.setAttribute('title', suBpRightTip(k));   /* 文案换了，tooltip 也得跟着换 */
  }
  if(cnt) cnt.textContent = '· ' + k.nRun + ' 项自启';
  if(sys){
    sys.style.width = (k.sysMs / den * 100).toFixed(2) + '%';
    sys.textContent = (k.sysMs / den * 100) >= 24 ? 'Windows 自身 ' + suSec(k.sysMs) + ' 秒' : '';
  }
  if(app){
    app.style.width = (k.appMs / den * 100).toFixed(2) + '%';
    /* 刀口挪动时，橙段里的字要跟着避让 —— 和 suBpHtml 里那段 padding 同一个算法 */
    app.style.paddingRight = (k.showCut && cutPct > (k.sysMs / den * 100))
      ? (100 - cutPct).toFixed(2) + '%' : '';
    app.textContent = (k.appMs / den * 100) >= 13 ? '启动项 ' + suSec(k.appMs) + ' 秒' : '';
    /* 悬停提示里的秒数也要跟着变，否则拨完开关指着条看，提示写着上一秒的数 */
    app.setAttribute('title', '启动项占用 ' + suSec(k.appMs)
      + ' 秒：本次开机被 Windows 点名、且咱们管得到的项');
  }
  if(cutbg && ct && ctxt){
    cutbg.style.left = cutPct.toFixed(2) + '%';
    ct.style.left = cutPct.toFixed(2) + '%';
    ctxt.style.left = cutPct.toFixed(2) + '%';
    ctxt.textContent = savePct >= 12 ? '省 ' + suSec(k.canSave) + ' 秒' : '';
  }
}

function suRenderPerf(quiet){
  var host = suEl('suPerf');
  if(!host) return;
  var b = SU_S.boot;
  var ok = !!(b && b.ok && b.records && b.records.length);
  var rec0 = ok ? b.records[0] : null;
  /* 读不到开机数据时清空标签表：SU_BOOT_TAG 是上一次算出来的，不清就会继续
     在列表里挂着「拖慢 21.3 秒」，而顶部明明写着"开机耗时读不到"。
     一个说没有数据、一个说有，用户只能判定其中一个是假的。 */
  if(!ok) SU_BOOT_TAG = {};
  var mainMs = rec0 ? Math.max(0, Number(rec0.mainPathMs) || 0) : 0;
  var postMs = rec0 ? Math.max(0, Number(rec0.postBootMs) || 0) : 0;

  /* 动作区先算：读不到开机数据时「开启日志」也挂在这里。 */
  var right = '';
  /* ⚠️ 判"有没有列表"用 items.length，不用 SU_S.loaded。
     点「重新扫描」时 suLoad 会先把 loaded 置 false，而 RefreshBootPerf 往往比
     ListStartupForce 先回来 —— 那一刻这里会算出 right=''，「一键优化 / 清理无效项」
     凭空消失几百毫秒再长回来（用户 2026-10-27 报的"按钮闪一下"就是它）。
     有旧数据就一直画按钮，转圈交给 suScanBusy。 */
  if(SU_S.loaded || (SU_S.items && SU_S.items.length)){
    var nOpt = suPlanList().length, nDel = suDel().length;
    if(nOpt || nDel){
      /* data-hot 已删（2026-10-27）：属性一直挂着，但 CSS 里从来没有对应的动画
         —— 只有一条"关闭动效时压制它"的孤立规则，等于给不存在的东西收尾。
         这块的强调已经由橙段 + 白线切口 + "关掉 N 项 → Y 秒"三重承担，
         再让按钮呼吸，反而是第三个抢注意力的东西，也违背"别闪"的整体要求。 */
      right = (nOpt ? '<button class="btn primary sm" id="suOptBtn"'
          + ' title="禁用这几项的开机自启，软件照常能用；随时能拨回来，也能在「优化记录」里一键恢复">'
          + ico('bolt', 13) + '<span>一键优化</span><b>' + nOpt + '</b></button>' : '')
        + (nDel ? '<button class="btn danger sm" id="suDelBtn"'
          + ' title="永久删除程序已卸载的残留项，删了找不回来；执行前会再让你确认一次">'
          + ico('trash', 13) + '<span>清理无效项</span><b>' + nDel + '</b></button>' : '');
    } else {
      right = '<span class="su-st-ok">' + ico('check', 13) + '没有需处理的项</span>';
    }
  }

  var main = '';
  if(ok){
    /* 整块只有两行：总时长 + 结算语，下面一条两段条。口径（从哪算起 /
       快速启动 / 取第几次）全部收进 .su-bp-top 的 title，不占版面。 */
    main = suBpHtml(suBpCalc());
    /* 数据在但算不出（mainPathMs 和 postBootMs 都是 0）时退回一句实话，
       不能留空白 —— 卡片空着比说"读不到"更让人以为软件坏了。 */
    if(!main) main = '<div class="su-st-hd">'
      + '<span class="su-st-ico warn">' + ico('info', 13) + '</span>'
      + '<span class="su-st-t">这次开机没有记录到耗时分段</span>'
      + '</div>';
  } else {
    /* 读不到就把原因说清楚（非管理员 / 日志被别的优化软件关了），不能整条消失 */
    var f = b && SU_BOOT_FIX[b.errCode];
    if(f){
      main = '<div class="su-st-hd">'
        + '<span class="su-st-ico warn">' + ico(f.ico, 13) + '</span>'
        + '<span class="su-st-t">开机耗时读不到</span>'
        + '<span class="su-st-meta">' + suEsc(b.err || f.txt) + '</span>'
        + '</div>';
      if(f.btn) right = '<button class="btn sm" id="suBootFix" data-fix="' + b.errCode + '">' + f.btn + '</button>' + right;
    }
  }

  /* quiet：整条状态条已经画好了，这次只是动作区的数字变了（一键优化 N / 清理无效项 N
     / 没有需处理的项）。只换那一小块，时间轴、三个数字、入场动画一律不动 ——
     重建 .su-strip 会让它重播 suStripIn（淡入 + 上移 6px），那就是"闪一下"的来源。
     ⚠️ 位置必须在算完 main 之后：「开启日志」按钮（#suBootFix）是在 main 那个
        分支里才追加进 right 的，提前换 innerHTML 会把它换丢 —— 非管理员机器上
        拨一下开关，「开启日志」就没了。
     ⚠️ 按钮走事件委托（#suOptBtn / #suDelBtn / #suBootFix 都在最外层 closest 里接），
        换 innerHTML 不用重绑事件；但 **disabled / busy 是写在元素上的状态，会丢**，
        所以换完要把重扫转圈补回去（见 suScanBusy 注释）。 */
  if(quiet){
    var r0 = host.querySelector('.su-st-r');
    if(r0 && host.querySelector('.su-strip')){
      if(!main && !right){ host.innerHTML = ''; return; }
      r0.innerHTML = right;
      /* 数字和两段宽度原地更新，不重建 .su-strip（重建会重播入场动画）。
         用户刚拨完开关，眼睛正盯在这块上：橙色段收窄、大数字滚下去、
         结算语从"最多省"变成"已关掉 N 项"，三处一起动才读得成因果。 */
      if(ok) suBpUpdate(suBpCalc());
      suScanBusy(SU_SCAN_BUSY);
      return;
    }
  }

  if(!main && !right){ host.innerHTML = ''; suScanBusy(SU_SCAN_BUSY); return; }
  /* has-bp：左侧是两行（数字行 + 条）时，卡片改成底对齐，让条和右侧按钮共用
     一条底边（详见 startup.css 里 .su-strip.has-bp 的说明）。 */
  var hasBp = main.indexOf('class="su-bp"') >= 0;
  host.innerHTML = '<div class="su-strip' + (hasBp ? ' has-bp' : '') + '">'
    + (main ? '<div class="su-st-main">' + main + '</div>' : '')
    + (right ? '<div class="su-st-r">' + right + '</div>' : '')
    + '</div>';

  if(ok){
    var sig = mainMs + '/' + postMs;
    if(sig !== SU_CU_SIG){ SU_CU_SIG = sig; suCountUp(); }
  }
  /* 重画出来的是全新按钮，disabled / busy 不会自己跟着来。重扫期间（含
     suForceLoad 里"先转圈、再刷开机数据"那一段）必须把它补回去，否则按钮
     看起来能点，点下去又是一次扫描。 */
  suScanBusy(SU_SCAN_BUSY);
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
  var bootOf = function(i){ return SU_BOOT_TAG[i.id] || 0; };
  return rows.slice().sort(function(a, b){
    if(s === 'name') return byName(a, b);
    if(s === 'state') return ((b.enabled ? 1 : 0) - (a.enabled ? 1 : 0)) || byName(a, b);
    /* 有实测拖慢秒数的排最前：这几行就是顶部那条橙色段的来源。用户看到
       「启动项 9.4 秒」往下找，第一眼就该看到是谁，而不是在几十行里翻。
       只在默认的「优化建议」排序里插队 —— 用户主动选了按名称/按状态，
       再插队就是跟他的手过不去。 */
    return (bootOf(b) - bootOf(a))
      || (suRank(a) - suRank(b))
      || ((b.enabled ? 1 : 0) - (a.enabled ? 1 : 0))
      || byName(a, b);
  });
}

function suHeadHtml(){
  var s = SU_S.sort || 'advice';
  var on = function(k){ return s === k ? ' on' : ''; };
  /* 表头用 .su-thead，和页头的 .su-head 分开（2026-10-27：同名同类互相污染，
     页头被套上表头的内边距和边框，标题与卡片差 10px 对不齐）。 */
  return '<div class="su-thead">'
    + '<span class="su-hico"></span>'
    + '<button class="su-h su-h-name' + on('name') + '" data-su-sort="name" title="按名称排序">程序名称</button>'
    + '<button class="su-h su-h-adv' + on('advice') + '" data-su-sort="advice" title="可手动优化的项目排在最前">优化建议</button>'
    + '<button class="su-h su-h-state' + on('state') + '" data-su-sort="state" title="按当前启用状态排序">当前状态</button>'
    + '<span class="su-hico"></span><span class="su-hico"></span></div>';
}

function suRowHtml(it){
  /* 三档建议：建议删除 / 建议禁用 / 保持现状；已停用的单独一档。
     ⚠️ 已停用的项不能再显示「建议禁用」—— 都已经禁用了还建议禁用，看着像软件没搞清楚状态。
     统一用「禁用」，不用「禁止」：Windows 任务管理器、服务管理器都是「禁用」，跟系统口径一致。 */
  var advB = it.advice === 'delete'
    ? '<span class="b del" title="' + suEsc(it.why || '这个启动项指向的程序已经不在了，建议清理掉') + '">建议删除</span>'
    : (it.advice === 'disable' && it.enabled
      ? '<span class="b yes" title="' + suEsc(it.why || '建议禁用：只取消开机自启，不影响软件功能，随时可恢复') + '">建议禁用</span>'
      : (!it.enabled
        /* 用「已禁用」而不是「已停用」：右侧状态列写的也是「已禁用」，
           同一个状态两个词，用户会以为它们是两回事。 */
        ? '<span class="b no" title="当前已禁用，需要时可随时恢复启用">已禁用</span>'
        : '<span class="b no" title="' + suEsc(it.why || '暂无优化建议，保持现状即可') + '">保持现状</span>'));
  var sysTag = it.sys ? '<i class="su-tag sys" title="系统自带组件，保持现状更稳妥">系统</i>' : '';
  /* 「拖慢 X 秒」只在 Windows 对本次开机点过名、且能在列表里对上号时才显示。
     没有数据的行一个字都不加 —— 满屏"未拖慢"会把真有数字的那两行淹掉，
     而这个标签的全部价值就是"让人看见"。 */
  /* slideMs 是 degradMs（"因为它，开机多等了多久"），不是 totalMs（"它自己跑了多久"）。
     v12 改字段时漏了这句 title，写成了"它跑了 X 秒" —— 数字对、话说错了，
     用户拿它跟"启动项占用"里的 totalMs 一比就对不上。 */
  var slowMs = SU_BOOT_TAG[it.id] || 0;
  var slowTag = slowMs > 0
    ? '<i class="su-tag slow" title="因为它，这次开机多等了 ' + suSec(slowMs)
      + ' 秒（Windows「诊断-性能」日志实测）。关掉它省下来的通常少于这个数，'
      + '因为启动项是并行跑的。">拖慢 ' + suSec(slowMs) + ' 秒</i>'
    : '';
  var sub = [it.desc || it.pub, (it.pub && it.desc) ? it.pub : '', it.loc].filter(Boolean).join(' · ');
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
    +   '<div class="su-nm" title="' + suEsc(it.cmd || '') + '"><span class="su-nmt">' + suEsc(it.name) + '</span>' + slowTag + sysTag + '</div>'
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

/* quiet = 这批行是「同一批数据再画一遍」（典型：拨了一次开关），不该重播行入场动画。
   见 startup.css 里 .su-list.anim-in 的注释：几十行一起淡入上移 = 整页在闪。 */
function suRenderList(quiet){
  var host = suEl('suList');
  if(!host) return;
  var rows = suRows();
  if(rows.length){
    host.innerHTML = '<div class="su-list' + (quiet ? '' : ' anim-in') + '">'
      + suHeadHtml() + rows.map(suRowHtml).join('') + '</div>';
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

function suRenderAll(quiet){
  if(typeof cur !== 'undefined' && cur !== 'sec') return;
  /* 每次进入这一页都让状态条重演一遍。原来只在数据首次回来时播一次 —— 那一刻
     窗口刚画出来，注意力还没落到这条上，等于没播；之后切回来又被"数据没变就不重播"
     挡掉，于是永远看不见。这里清掉签名，让下面这条重新数一遍。
     ⚠️ 但重演只属于"进入这一页"；拨一次开关也走这里的话，整条状态条会重播一遍
     入场动画（淡入 + 上移 6px），用户看到的就是"整个卡片闪一下"。
     quiet = 只更新跟着这次操作变的东西（按钮计数、那一行），不重播任何入场。 */
  if(!quiet) SU_CU_SIG = '';
  /* 非管理员提示条： #suTip 会随页面重绘被清空，而 suLoadDiag 只在"首次加载"时跑 ——
     切走再回来（或在别的页点过重新扫描）提示就再也不出现了，SU_S.admin 却还在内存里。
     每次渲染都补一次，幂等、无动效。 */
  suRenderTip();
  suSyncSysChk(); suRenderPerf(quiet);
  if(!quiet) suRenderTabs();          /* 分类计数与启用状态无关，拨开关不会让它变 */
  suRenderList(quiet);
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
  /* 只把色相传给 CSS，亮度和饱和度交给样式表按主题定。
     以前把 hsl(… 92%) 写死在内联 style 里：深色主题下列表左边一排近白方块，
     而且切主题时这些内联值不会重算（列表不重绘就一直是亮的）。 */
  return '<span class="fb-l" style="--fbh:' + hue + '">' + suEsc(ch) + '</span>';
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
    suRenderAll(true);   /* quiet：只换动作区数字 + 重排列表，别让整条状态条重播入场动画 */
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
/* 上一个行内菜单挂在 document 上的"点外面关闭"监听。
   以前每开一次菜单就 addEventListener 一个、且只有"点了外面"才解绑 ——
   用户连开三个菜单、最后用 ⋮ 之外的区域关掉，前两个监听还挂在 document 上。
   数量不多不会崩，但它们是闭包，抓着已经 remove 的节点不放。开新菜单时先解旧的。 */
var SU_MENU_OFF = null;
function suMenu(btn){
  suEach('.su-menu', function(m){ m.remove(); });
  if(SU_MENU_OFF){ SU_MENU_OFF(); SU_MENU_OFF = null; }
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
    var close = function(ev){
      if(m.contains(ev.target)) return;
      m.remove();
      if(SU_MENU_OFF){ SU_MENU_OFF(); SU_MENU_OFF = null; }
    };
    document.addEventListener('click', close);
    SU_MENU_OFF = function(){
      document.removeEventListener('click', close);
      SU_MENU_OFF = null;
    };
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
/* 弹窗底部那个「关闭 / 好的」按钮（data-act="c"）。六个弹窗都写一遍
   querySelector + onclick，漏一处那个弹窗就关不掉。 */
function suModalCloseBind(){
  var box = suEl('modalBox');
  var c = box ? box.querySelector('[data-act="c"]') : null;
  if(c) c.onclick = suModalClose;
  return c;
}
/* 批量动作的进度页。一键优化和清理无效项除了标题、说明，结构一模一样 ——
   以前两处各抄了一份，改一处忘另一处就是两套进度条。 */
function suProgHtml(iconName, title, sub, done, total){
  return '<div class="modal-t">' + ico(iconName, 18) + title + '</div>'
    + '<div class="modal-d">' + sub + '</div>'
    + '<div class="su-prog"><div class="su-prog-b" style="width:'
    + Math.round(done / Math.max(1, total) * 100) + '%"></div></div>'
    + '<div class="su-prog-t">' + done + ' / ' + total + '</div>';
}
/* 批量动作做完的那一页（勾 + 标题 + 一句结论 + 「好的」）。 */
function suDoneHtml(title, para){
  return '<div class="su-done"><div class="su-done-ico">' + ico('check', 28) + '</div>'
    + '<h3>' + title + '</h3><p>' + para + '</p>'
    + '<button class="btn primary" data-act="c">好的</button></div>';
}

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
    suModalCloseBind();
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
    suModalCloseBind();
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
/* 清单来自 suPlanList()：实测点名的排最前（带秒数徽章、默认勾上），
   后面是我们按程序性质判定的（也默认勾上）。 */
function suOptModal(){
  var plan = suPlanList();
  if(!plan.length){ toast('当前没有需要优化的项目', 'info'); return; }
  var nHot = plan.filter(function(x){ return x.hot; }).length;
  /* 顶部原来还有一句"全部勾上，开机约快 X 秒" —— 2026-10-27 删掉：
     它是静态算出来的，用户取消勾选最大那项之后数字纹丝不动，等于在骗人。
     现在只留底部那行"已选 N 项 · 开机约快 X 秒"，勾哪儿变哪儿。 */
  suModal('<div class="modal-t">' + ico('bolt', 18) + '一键优化 · ' + plan.length + ' 项</div>'
    + '<div class="modal-d">勾选要禁用的项 —— 只是不再开机自启，软件功能不受影响，随时可恢复'
    + (nHot ? '。<b>带秒数的是 Windows 实测拖慢开机的项，已经帮你勾上，不想关的取消即可</b>' : '')
    + '</div>'
    + '<div class="su-optlist" id="suOptList">'
    + plan.map(function(p){
        var it = p.it;
        var kindIco = it.kind === 'task' ? 'clock' : (it.kind === 'svc' ? 'cpu' : 'box');
        return '<label class="su-optrow' + (p.hot ? ' hot' : '') + '">'
          + '<input type="checkbox" data-su-opt="' + suEsc(it.id) + '" data-su-ms="' + (p.ms || 0) + '"'
          + (p.on ? ' checked' : '') + '>'
          + '<div class="su-ico" data-su-ico="' + suEsc(it.iconKey || '') + '">' + ico(kindIco, 16) + '</div>'
          + '<div class="su-main">'
          +   '<div class="su-nm"><span class="su-nmt">' + suEsc(it.name) + '</span>'
          +     '<i class="su-tag kind">' + (SU_KIND[it.kind] || it.kind) + '</i>'
          +     (p.hot ? '<i class="su-tag hot" title="Windows 实测：它在 ' + suEsc(p.day)
                 + ' 那次开机里，让你多等了 ' + suSec(p.ms) + ' 秒">拖慢 ' + suSec(p.ms) + ' 秒</i>' : '')
          +   '</div>'
          +   '<div class="su-sub" title="' + suEsc(it.why || '') + '">'
          +     suEsc(it.desc || it.pub || '无描述信息') + (it.why ? ' · ' + suEsc(it.why) : '') + '</div>'
          + '</div></label>';
      }).join('')
    + '</div>'
    + '<div class="su-optbar"><span id="suOptCnt"></span>'
    + '<div class="su-optbtns"><button class="btn" data-act="c">取消</button>'
    + '<button class="btn primary" id="suOptGo">' + ico('bolt', 14) + '立即优化</button></div></div>');
  var upd = function(){
    var el = suEl('suOptCnt');
    if(!el) return;
    var n = 0, save = 0;
    suEach('#suOptList [data-su-opt]', function(c){
      if(!c.checked) return;
      n++;
      save += Number(c.getAttribute('data-su-ms')) || 0;
    });
    el.textContent = '已选 ' + n + ' / ' + plan.length + ' 项'
      + (save >= 500 ? ' · 开机约快 ' + suSec(save) + ' 秒' : '');
  };
  suEach('#suOptList [data-su-opt]', function(c){ c.onchange = upd; });
  upd();
  suModalIcons();
  suModalCloseBind();
  var g = suEl('suOptGo');
  if(g) g.onclick = suOptRun;
}
function suOptRun(){
  var ids = [];
  suEach('#suOptList [data-su-opt]:checked', function(c){ ids.push(c.getAttribute('data-su-opt')); });
  if(!ids.length){ toast('请先勾选要优化的项目', 'danger'); return; }
  var done = 0, fail = 0;
  var prog = function(){
    return suProgHtml('bolt', '正在优化…', '正在逐项禁用，请不要关闭窗口', done, ids.length);
  };
  suModal(prog());
  (function step(){
    if(done >= ids.length){
      suSyncGlobal();
      /* ⚠️ 不能写"下次开机明显提速" —— 提速多少无法承诺，用户重启发现没变
         就会认为软件在骗人。只陈述已经确定的事实：它们不再开机自启。 */
      suModal(suDoneHtml('优化完成',
        (ids.length - fail) + ' 项已禁用' + (fail ? ('，' + fail + ' 项处理失败') : '')
        + '。<br>它们不会再开机自动运行；反悔了随时在「优化记录」里恢复。'));
      suModalCloseBind();
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

/* ── 清理无效项（批量删除，不可恢复 → 必须二次确认）───────────────────────
   和「一键优化」分开成一个独立按钮，是因为这两个动作的后果完全不同：
   禁用 = 改状态，随时能开回来；删除 = 真删掉，从这里再也找不回来。
   ⚠️ 服务项不进来：suDelete 对 svc 直接返回错误（删错服务会让系统组件失效），
   失效服务在后端已经被改成 advice=disable，走「一键优化」那条路。 */
function suCleanModal(){
  var dels = suDel();
  if(!dels.length){ toast('没有需要清理的无效项', 'info'); return; }
  suModal('<div class="modal-t">' + ico('trash', 18) + '清理无效项 · ' + dels.length + ' 项</div>'
    + '<div class="modal-d">这些项指向的程序已经不在了，留着也不会再启动。'
    + '<b>删除后不可恢复</b> —— 只是想让它不开机运行的话，用「一键优化」禁用就行。</div>'
    + '<div class="su-optlist" id="suDelList">'
    + dels.map(function(it){
        var kindIco = it.kind === 'task' ? 'clock' : (it.kind === 'svc' ? 'cpu' : 'box');
        return '<label class="su-optrow">'
          + '<input type="checkbox" data-su-del="' + suEsc(it.id) + '" checked>'
          + '<div class="su-ico" data-su-ico="' + suEsc(it.iconKey || '') + '">' + ico(kindIco, 16) + '</div>'
          + '<div class="su-main">'
          +   '<div class="su-nm"><span class="su-nmt">' + suEsc(it.name) + '</span>'
          +     '<i class="su-tag kind">' + (SU_KIND[it.kind] || it.kind) + '</i></div>'
          +   '<div class="su-sub" title="' + suEsc(it.cmd || '') + '">' + suEsc(it.why || '程序已经不在了') + '</div>'
          + '</div></label>';
      }).join('')
    + '</div>'
    + '<div class="su-optbar"><span id="suDelCnt"></span>'
    + '<div class="su-optbtns"><button class="btn" data-act="c">取消</button>'
    + '<button class="btn danger" id="suDelGo">' + ico('trash', 14) + '确认删除</button></div></div>');
  var upd = function(){
    var el = suEl('suDelCnt');
    if(el) el.textContent = '已选 ' + document.querySelectorAll('#suDelList [data-su-del]:checked').length + ' / ' + dels.length + ' 项';
  };
  suEach('#suDelList [data-su-del]', function(c){ c.onchange = upd; });
  upd();
  suModalIcons();
  suModalCloseBind();
  var g = suEl('suDelGo');
  if(g) g.onclick = suCleanRun;
}
function suCleanRun(){
  var ids = [];
  suEach('#suDelList [data-su-del]:checked', function(c){ ids.push(c.getAttribute('data-su-del')); });
  if(!ids.length){ toast('请先勾选要清理的项目', 'danger'); return; }
  /* 二次确认：删除是唯一的不可逆动作，一次手滑就永久丢一条。
     ⚠️ 这里不用 askThenDo（2026-10-27）：它会**占用同一个 #modalBox**，点"取消"
     就直接 closeModal —— 刚才勾好的一整张清理清单被吃掉了，用户得从按钮重来一遍。
     自己画这一步，"取消"能退回清单、勾选还在。 */
  suModal('<div class="modal-t">确认删除这 ' + ids.length + ' 项？</div>'
    + '<div class="modal-d" style="line-height:1.8">删除后本软件无法帮你恢复。'
    + '<br>如果只是不想让它开机运行，返回去用「一键优化」禁用更安全 —— 那个随时能开回来。</div>'
    + '<div class="modal-f"><button class="btn" id="suDelNo">返回清单</button>'
    + '<button class="btn danger" id="suDelYes">' + ico('trash', 14) + '永久删除</button></div>');
  var no = suEl('suDelNo');
  if(no) no.onclick = suCleanModal;      /* 回到上一步，勾选状态重算 */
  var yes = suEl('suDelYes');
  if(yes) yes.onclick = function(){
    var done = 0, fail = 0;
    var prog = function(){
      return suProgHtml('trash', '正在清理…', '正在逐项删除，请不要关闭窗口', done, ids.length);
    };
    suModal(prog());
    (function step(){
      if(done >= ids.length){
        suSyncGlobal();
        suModal(suDoneHtml('清理完成',
          (ids.length - fail) + ' 项已删除' + (fail ? ('，' + fail + ' 项失败') : '') + '。'));
        suModalCloseBind();
        suForceLoad();
        return;
      }
      var id = ids[done];
      callGo('DeleteStartup', id).then(function(){}, function(){ fail++; }).then(function(){
        done++;
        suModal(prog());
        step();
      });
    })();
  };
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
  if((el = t.closest('#suDelBtn'))){ suCleanModal(); return; }
  if((el = t.closest('#suBootFix'))){ suBootFix(el.getAttribute('data-fix')); return; }
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

/* ⚠️ 必须挂 window：加载器是 IIFE 里 eval，主文件的 PAGES 表要引用 renderStartup。
   只有这四个有外部消费者（PAGES / 进页绑定 / 刷新映射 / 回归脚本读状态），
   其余一律留在模块内 —— 多挂一个就多一个改了不敢动的隐式接口。 */
window.renderStartup = renderStartup;
window.bindStartup   = bindStartup;
window.suLoad        = suLoad;
window.SU_S          = SU_S;
