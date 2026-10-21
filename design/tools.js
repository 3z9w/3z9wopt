/* ══════════════════════════════════════════════════════════════════════════
   工具库 · 独立模块（design/tools.js）
   ---------------------------------------------------------------------------
   功能清单照老版本（后端 toolkit.go + tools_*.go，34 个工具 / 5 类）。
   布局 2026-10-12 第二次重做（第一版三栏被否：左「分类竖列表 + 工具清单竖列表」
   两个同构列表叠在一起层级打架；右侧详情大片留白；输出区常驻底部又只有 200px）：

     现在的结构 = 一页卡片墙 + 运行时才弹的终端窗口
       · 分类从左侧竖列表改成**顶部横排页签**（一级，不再和工具清单抢层级）
       · 工具**卡片自带一句话说明 + 命令 + 运行按钮**，不用点进去才知道是干嘛的
       · 页面**不再给输出留常驻地盘**：点「运行」弹居中终端窗口，参数、输出、
         停止 / 复制 / 清空都在窗口里；窗口可最小化（工具继续跑），最小化后
         左下角留悬浮条，点「查看输出」回到窗口，输出不丢
       · 危险工具（danger）不弹两层窗口 —— 在终端窗口底部变成确认条（确认 / 取消）

   后端接口：GetToolCats / GetToolCatalog / GetDrives / GetLanShareInfo /
             GetInstalledUpdates / GetPendingReboot / RunTool / StopTool / CopyText
   事件：tool:log(kind,text) → tkOnLog；tool:done(code,secs,msg) → tkOnDone
   ⚠️ 对外入口全部挂 window（主文件是同步 XHR + eval 的 IIFE 加载的）；
      主文件里的 cur / TOOLS 之类是 script 作用域局部变量，模块读不到 ——
      判断"在不在本页"一律看 #tkWall 在不在，不看 cur。
   ⚠️ 弹窗 #modalBox 在 #content 之外：交互绑定要同时挂 #content 和 #modalBox。
   ══════════════════════════════════════════════════════════════════════════ */

var TK = {
  state: 'loading',   // loading | ready | failed
  err: '',
  cats: [], tools: [], drives: [], lan: null,
  cat: 'netfix',              /* 默认落在第一个症状组，不默认铺全部（铺开会太密） */
  run: null, busy: false,
  loading: false, seq: 0,   // loading 防重复拉；seq 用于作废上一轮的超时兜底
  updates: null, pending: false,
  term: '', termOpen: false, cf: null,   // cf = 危险工具的待确认 {id,arg}
  a1: null, a2: null, a3: null,          // 参数回填（重画窗口时保住已填的值）
  /* 对症排查（2026-10-13 从右上角菜单那个独立页面搬进来）：
     症状 → 步骤 → 一键跑工具。跟工具库同源，就放在工具库页顶部，默认收起。 */
  guides: [], guideState: 'loading', guideErr: '', guideOpen: {}, guideLoading: false, guideFold: true
};
window.TK = TK;

/* ── 小工具 ─────────────────────────────────────────────────────────────── */
/* 当前是不是在工具库页（#tkWall 只在渲染成功后才有；loading/failed 页没有它，
   所以补一条"页标题是工具库"的判断 —— 主文件的 cur 是 script 局部变量，模块读不到） */
function tkOnPage(){
  if(tkEl('tkWall')) return true;
  var t = document.querySelector('.page-title');
  return !!(t && String(t.textContent || '').indexOf('工具库') >= 0);
}
function tkEsc(s){
  return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];
  });
}
function tkEl(id){ return document.getElementById(id); }
function tkById(id){
  for(var i = 0; i < TK.tools.length; i++){ if(TK.tools[i].id === id) return TK.tools[i]; }
  return null;
}
var TK_CATICO = {fix:'shield', net:'wifi', disk:'disk', info:'monitor', util:'grid'};
function tkCatIco(k){ return TK_CATICO[k] || 'box'; }
/* ── 症状分组（2026-10-13 定：34 个工具平铺成卡片墙太密，改成按"你遇到的毛病"分组）
   按工具 ID 归类，不是按后端的技术分类（sfc / dism 这种只有工程师才认得）。
   ⚠️ 数量全部按 TK.tools 实时数（后端少一个工具也不会虚标）；
   ⚠️ 没写进任何一组的工具自动落到「其它」，不会因为映射写漏就丢功能。 */
var TK_SCENES = [
  {key:'netfix', name:'上不了网',   ico:'wifi',    desc:'网页打不开、网速慢、连不上局域网',
   ids:['net-smart','net-ping','net-tracert','net-speed','net-port','net-hosts','net-lanfix']},
  {key:'update', name:'更新装不上', ico:'refresh', desc:'Windows 更新失败、补丁想卸掉、应用商店打不开',
   ids:['fix-update','fix-update-list','fix-update-remove','fix-store','info-updates']},
  {key:'broken', name:'系统报错',   ico:'shield',  desc:'缺 dll、功能异常、系统文件损坏；动手前先留后路',
   ids:['fix-sfc','fix-sfc-verify','fix-dism-restore','fix-dism-scan','fix-restore-point']},
  {key:'boot',   name:'开不了机',   ico:'box',     desc:'启动失败、引导丢失、卡在开机画面',
   ids:['fix-boot','fix-bcd']},
  {key:'disk',   name:'磁盘异常',   ico:'disk',    desc:'硬盘变慢、读写出错、怀疑有坏道',
   ids:['disk-defrag-analyze','disk-defrag-optimize','disk-smart','disk-speed','fix-chkdsk']},
  {key:'info',   name:'查本机情况', ico:'monitor', desc:'看看硬件、电池、驱动、开机多久了',
   ids:['info-hw','info-battery','info-drivers','info-uptime']},
  {key:'daily',  name:'日常小工具', ico:'grid',    desc:'定时关机、打印机卡住、时间不对、激活状态',
   ids:['util-shutdown','util-shutdown-cancel','util-spooler','util-timesync','util-activation','util-opentool']}
];
function tkScene(k){
  for(var i = 0; i < TK_SCENES.length; i++){ if(TK_SCENES[i].key === k) return TK_SCENES[i]; }
  return null;
}
/* 这个工具属于哪个症状组（Ctrl+K 跳转要用；没归类的落 'other'） */
function tkSceneOf(id){
  for(var i = 0; i < TK_SCENES.length; i++){
    if(TK_SCENES[i].ids.indexOf(id) >= 0) return TK_SCENES[i].key;
  }
  return 'other';
}
/* 数量按实际读到的工具数 —— 后端少给一个，页签上的数字就得跟着少 */
function tkCnt(k){
  if(k === 'all') return TK.tools.length;
  if(k === 'other'){
    var n0 = 0;
    for(var j = 0; j < TK.tools.length; j++){ if(tkSceneOf(TK.tools[j].id) === 'other') n0++; }
    return n0;
  }
  var s = tkScene(k), n = 0;
  if(!s) return 0;
  for(var i = 0; i < TK.tools.length; i++){ if(s.ids.indexOf(TK.tools[i].id) >= 0) n++; }
  return n;
}
function tkFiltered(){
  var out = [];
  for(var i = 0; i < TK.tools.length; i++){
    var id = TK.tools[i].id;
    if(TK.cat === 'all'){ out.push(TK.tools[i]); continue; }
    if(tkSceneOf(id) === TK.cat) out.push(TK.tools[i]);
  }
  return out;
}
/* 点卡片上的「运行」是否直接开跑（需要交互/危险/必填参数的一律先开窗口等人） */
function tkAutoRun(t){
  if(!t || t.argReq || t.danger) return false;
  var k = t.argKind;
  if(k === 'netpanel' || k === 'updatelist' || k === 'shutdown' || k === 'speedtest') return false;
  return true;
}

/* ── 数据 ───────────────────────────────────────────────────────────────── */
function tkLoad(){
  if(TK.loading) return;                     /* 别并发拉两份 */
  if(!hasGo()){
    /* 后端绑定还没注入（启动时序常见）：不许静默留在 loading —— 那会永远转圈，
       界面上一个字也不给。直接置 failed，自愈成功后会重跑 tkLoad 覆盖掉。 */
    TK.state = 'failed'; TK.err = '后端连接还没就绪（window.go 为空）';
    /* ⚠️ 门闩：下面 render() 会回到本函数（页面切换入口 tools->tkLoad），
       不置 loading 就会 render→load→render 无限递归爆栈（2026-10-13 实测 RangeError）。 */
    TK.loading = true;
    if(tkOnPage() && typeof render === 'function') render();
    TK.loading = false;
    return;
  }
  TK.loading = true; TK.state = 'loading'; TK.err = '';
  TK.seq++; var my = TK.seq;
  /* 超时兜底：Promise 一直不 settle 时页面会永远停在「正在读工具目录…」，
     给个明确说法 + 重新读取按钮，别让用户干等。 */
  var guard = setTimeout(function(){
    if(my !== TK.seq) return;
    TK.loading = false; TK.state = 'failed'; TK.err = '读取超时：后端 8 秒没回应';
    if(tkOnPage() && typeof render === 'function') render();
  }, 8000);
  var done = function(){ TK.seq++; clearTimeout(guard); TK.loading = false; };
  Promise.all([callGo('GetToolCats'), callGo('GetToolCatalog'), callGo('GetDrives')]).then(function(r){
    done();
    TK.cats = r[0] || []; TK.tools = r[1] || []; TK.drives = r[2] || [];
    /* 默认场景一台机器可能一个工具都没有（比如 net 类全被精简掉）→ 退回「全部」，别显示空页 */
    if(!tkCnt(TK.cat) && TK.tools.length) TK.cat = 'all';
    TK.state = 'ready';
    /* 只有当前已经在工具库页才重画（#tkWall 在 = 本页已渲染）。
       ⚠️ 但"停在 loading 页"时页面里根本没有 #tkWall —— 只看 tkWall 会导致
       数据到了也不重画，页面永远转圈（2026-10-12 实测就是这个）。
       所以落在 loading / failed 页上时必须整页 render()。 */
    if(tkEl('tkWall')){ tkPaintHero(); tkPaintTabs(); tkPaintWall(); }
    else if(tkOnPage() && typeof render === 'function') render();
    callGo('GetPendingReboot').then(function(st){
      TK.pending = (st === null || st === undefined) ? false
        : (typeof st === 'number' ? st > 0 : !!(st.n > 0 || st.N > 0));
      if(tkEl('tkWall')) tkPaintReboot();
    }).catch(function(){});
  }).catch(function(e){
    done();
    TK.state = 'failed'; TK.err = String(e);
    if(tkOnPage() && typeof render === 'function') render();
  });
}
window.tkLoad = tkLoad;

/* ── 页面骨架 ───────────────────────────────────────────────────────────── */
function renderTools(){
  var h = '<div class="page-head"><div class="page-title">工具库</div>'
    + '<div class="page-sub">按你遇到的毛病挑工具，认准了就点卡片上的「运行」。</div></div>';

  if(TK.state === 'failed'){
    return h + '<div class="card"><div class="empty" style="padding:48px 20px">'
      + '<div class="empty-ico" style="color:var(--danger)">' + ico('warn', 28) + '</div>'
      + '<div class="empty-t">没能读到工具目录</div><div class="empty-d">' + tkEsc(TK.err) + '</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      + '<button class="btn primary" data-tkact="retry">' + ico('refresh', 16) + '重新读取</button></div>'
      + '</div></div>';
  }
  if(TK.state !== 'ready'){
    return h + '<div class="card"><div class="empty" style="padding:56px 20px">'
      + '<div class="empty-ico" style="color:var(--info)">' + ico('refresh', 28) + '</div>'
      + '<div class="empty-t">正在读工具目录…</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      + '<button class="btn" data-tkact="retry">' + ico('refresh', 16) + '重新读取</button></div>'
      + '</div></div>';
  }

  /* hero：一句话说明 + 右侧 KPI（数量真从后端目录数，不写死） */
  h += '<div class="tk-hero" id="tkHero">' + tkHeroHtml() + '</div>';
  /* 对症排查：放在 hero 与分类页签之间 —— 一眼看得见，又不把工具墙顶下去 */
  h += '<div id="tkFlowBox">' + tkFlowHtml() + '</div>';

  h += '<div class="tk-tabs" id="tkTabs">' + tkTabsHtml()
    + '<button class="tk-reload" data-tkact="rerun">' + ico('refresh', 13) + '重新读取</button></div>';
  h += '<div id="tkRebootBox">' + tkRebootHtml() + '</div>';
  /* 运行中状态条放在卡片墙上方（页内流式，不用 fixed：
     #content 有 backdrop-filter，fixed 会被它当成包含块，跑到内容区左下角压住卡片）。 */
  h += '<div id="tkMiniBox">' + tkMiniHtml() + '</div>';
  h += '<div class="tk-wall" id="tkWall">' + tkWallHtml() + '</div>';
  return h;
}
window.renderTools = renderTools;

/* 局部重绘（切分类/搜索/选中都不整页 render —— 整页重绘会吞点击） */
function tkPaintHero(){ var b = tkEl('tkHero'); if(b) b.innerHTML = tkHeroHtml(); }
function tkPaintTabs(){ var b = tkEl('tkTabs'); if(b) b.innerHTML = tkTabsHtml() + '<button class="tk-reload" data-tkact="rerun">' + ico('refresh', 13) + '重新读取</button>'; }
function tkPaintWall(){ var b = tkEl('tkWall'); if(b) b.innerHTML = tkWallHtml(); }
function tkPaintReboot(){ var b = tkEl('tkRebootBox'); if(b) b.innerHTML = tkRebootHtml(); }
function tkPaintMini(){ var b = tkEl('tkMiniBox'); if(b) b.innerHTML = tkMiniHtml(); }

/* ════════ 对症排查（2026-10-13 从独立的「快速排查」页搬进来）════════
   原来它挂在右上角菜单的 go('flow')，是个谁也不容易发现的隐藏页。
   右上角菜单整个删掉后搬到这里：症状清单本质就是「不知道该用哪个工具 → 照着做 →
   一键执行」，和工具库同源，放工具库页顶部最顺。
   ⚠️ 默认**收起**（guideFold）：工具库本身已经很长，铺开会把下面的工具墙顶到屏幕外。
   展开/收起、每条症状的展开都只重画 #tkFlowBox 这一个盒子 —— 整页 render 会吞点击。 */
function tkGuideName(id){
  for(var i = 0; i < TK.tools.length; i++){ if(TK.tools[i].id === id) return TK.tools[i].name; }
  return id;
}
function tkLoadGuides(){
  if(TK.guideLoading) return;
  if(!hasGo()){ TK.guideState = 'failed'; TK.guideErr = '后端连接还没就绪（window.go 为空）'; tkPaintFlow(); return; }
  TK.guideLoading = true; TK.guideState = 'loading'; TK.guideErr = '';
  callGo('GetGuides').then(function(l){
    TK.guideLoading = false; TK.guides = l || []; TK.guideState = 'ready'; tkPaintFlow();
  }).catch(function(e){
    TK.guideLoading = false; TK.guideState = 'failed'; TK.guideErr = String(e); tkPaintFlow();
  });
}
function tkFlowAct(g){
  if(g.toolId){
    for(var i = 0; i < TK.tools.length; i++){ if(TK.tools[i].id === g.toolId){ tkJump(g.toolId); return; } }
    /* 工具目录还没到 / 这台机器没这个工具：明说，别假装能跑 */
    toast('工具库里没有「' + tkGuideName(g.toolId) + '」这个工具，先在下面找找看', 'warn');
    return;
  }
  if(g.pageId){ if(typeof go === 'function') go(g.pageId); return; }
  toast('这一条没有一键动作，照着上面的步骤做即可', 'info');
}
function tkFlowHtml(){
  var h = '<div class="tk-flow">'
    + '<div class="tk-flow-h" data-flfold="1">'
    +   '<span class="tk-flow-ico">' + ico('sparkle', 15) + '</span>'
    +   '<b>对症排查</b>'
    +   '<span class="tk-flow-n">'
    +     (TK.guideState === 'ready' ? (TK.guides.length + ' 个症状') : (TK.guideState === 'failed' ? '没读到' : '读取中…'))
    +   '</span>'
    +   '<span style="margin-left:auto"></span>'
    +   '<span class="tk-flow-x">' + (TK.guideFold ? '展开' : '收起') + '</span>'
    + '</div>';
  if(TK.guideFold) return h + '</div>';

  if(TK.guideState === 'failed'){
    return h + '<div class="tk-flow-b"><div class="tk-flow-err">没能读到自查清单：' + tkEsc(TK.guideErr) + '</div>'
      + '<button class="btn sm" data-flretry="1">' + ico('refresh', 13) + '重新读取</button></div></div>';
  }
  if(TK.guideState !== 'ready'){
    return h + '<div class="tk-flow-b"><div class="tk-flow-err">正在读自查清单…</div></div></div>';
  }
  if(!TK.guides.length){
    return h + '<div class="tk-flow-b"><div class="tk-flow-err">后端没有给自查清单。不影响使用，直接在下面按名字找工具就行。</div></div></div>';
  }
  h += '<div class="tk-flow-b">' + TK.guides.map(function(g){
    var open = !!TK.guideOpen[g.id];
    var act = g.toolId ? ('一键：' + tkGuideName(g.toolId)) : (g.pageId ? (g.actLabel || '去处理') : '');
    return '<div class="tk-flow-i">'
      + '<div class="tk-flow-t' + (act ? ' hasact' : '') + '" data-fltog="' + tkEsc(g.id) + '">'
      +   '<b>' + tkEsc(g.title) + '</b>'
      +   '<span class="chip">' + ((g.steps || []).length) + ' 步</span>'
      +   '<span style="margin-left:auto"></span>'
      +   (act ? ('<button class="btn sm primary" data-flact="' + tkEsc(g.id) + '">' + tkEsc(act) + '</button>') : '')
      +   '<span class="tk-flow-x">' + (open ? '收起' : '展开') + '</span>'
      + '</div>'
      + (open ? ('<div class="tk-flow-s">'
          + (g.steps || []).map(function(s, i){
              return '<div class="tk-flow-step"><span>' + (i + 1) + '.</span><span>' + tkEsc(s) + '</span></div>';
            }).join('')
          + (g.article ? ('<div class="tk-flow-art">相关文章：'
              + '<button class="btn sm" data-flart="' + tkEsc(g.article) + '" data-arttitle="' + tkEsc(g.articleTitle || '') + '">'
              + tkEsc(g.articleTitle || g.article) + '（复制链接）</button></div>') : '')
          + '</div>') : '')
      + '</div>';
  }).join('') + '</div></div>';
  return h;
}
function tkPaintFlow(){ var b = tkEl('tkFlowBox'); if(b) b.innerHTML = tkFlowHtml(); }
/* 探针与调试要用：模块在 eval 作用域里，外面够不着 tkPaintFlow（2026-10-13） */
window.tkPaintFlow = tkPaintFlow;

/* ── hero：一句话说明 + 右侧 KPI（数量 = 后端目录实数值） ───────────────── */
function tkHeroHtml(){
  var rec = 0, scene = 0;
  for(var i = 0; i < TK.tools.length; i++){ if(TK.tools[i].recommend) rec++; }
  for(var j = 0; j < TK_SCENES.length; j++){ if(tkCnt(TK_SCENES[j].key)) scene++; }
  return '<div class="tk-hero-ico">' + ico('grid', 21) + '</div>'
    + '<div class="tk-hero-main">'
    +   '<div class="tk-hero-t">点「运行」是真跑，输出在弹出窗口里</div>'
    +   '<div class="tk-hero-d">窗口可<b>最小化</b>（工具继续跑），最小化后页顶留一条状态条，点「查看输出」回来，输出不丢；'
    +     '会改动系统的工具执行前会先问一句。</div>'
    + '</div>'
    + '<div class="tk-kpis">'
    +   '<div class="tk-kpi"><span>工具</span><b>' + TK.tools.length + '</b></div>'
    +   '<div class="tk-kpi"><span>按症状分</span><b>' + scene + '</b></div>'
    +   '<div class="tk-kpi"><span>常用推荐</span><b>' + rec + '</b></div>'
    + '</div>';
}

/* ── 分类页签（横排一级，取代原来的左侧竖列表） ─────────────────────────── */
function tkTabsHtml(){
  var h = TK_SCENES.map(function(s){
    var n = tkCnt(s.key);
    if(!n) return '';      /* 这台机器上这个症状没有对应工具就不显示页签 */
    return '<button class="tk-tab' + (TK.cat === s.key ? ' on' : '') + '" data-tkcat="' + s.key + '">'
      + ico(s.ico, 14)
      + '<span>' + tkEsc(s.name) + '</span>'
      + '<span class="tk-tab-c">' + n + '</span></button>';
  }).join('');
  var o = tkCnt('other');
  if(o){
    h += '<button class="tk-tab' + (TK.cat === 'other' ? ' on' : '') + '" data-tkcat="other">'
      + ico('box', 14) + '<span>其它</span><span class="tk-tab-c">' + o + '</span></button>';
  }
  h += '<button class="tk-tab' + (TK.cat === 'all' ? ' on' : '') + '" data-tkcat="all">'
    + '<span>全部</span><span class="tk-tab-c">' + TK.tools.length + '</span></button>';
  return h;
}

/* ── 卡片墙（按分类分组；搜索/筛选时并成一屏） ─────────────────────────── */
function tkWallHtml(){
  var list = tkFiltered();
  if(!list.length){
    return '<div class="card"><div class="tk-none">这一类还没有工具。</div></div>';
  }
  /* 一层分组就够（症状本身已经是分类），不再套后端的技术分类 → 不再层层堆叠 */
  var s = tkScene(TK.cat);
  var head;
  if(s){
    head = '<div class="tk-grp-h">' + ico(s.ico, 14) + '<b>' + tkEsc(s.name) + '</b>'
      + '<span>' + list.length + ' 个</span><span class="tk-grp-d">' + tkEsc(s.desc) + '</span></div>';
  }else if(TK.cat === 'other'){
    head = '<div class="tk-grp-h">' + ico('box', 14) + '<b>其它</b><span>' + list.length + ' 个</span></div>';
  }else{
    head = '<div class="tk-grp-h">' + ico('box', 14) + '<b>全部工具</b><span>' + list.length + ' 个</span>'
      + '<span class="tk-grp-d">按上面的症状找更快</span></div>';
  }
  return '<div class="tk-grp">' + head + '<div class="tk-grid">' + list.map(tkCardHtml).join('') + '</div></div>';
}

function tkCardHtml(t){
  var b = '';
  if(t.recommend) b += '<i class="tk-b rec">荐</i>';
  if(t.long) b += '<i class="tk-b">耗时较长</i>';
  if(t.reboot) b += '<i class="tk-b">需重启</i>';
  /* 标题一行放不下就折行（.tk-c-t 允许换行），不截断 —— 工具名看全才好找 */
  return '<div class="tk-card" data-tkopen="' + tkEsc(t.id) + '">'
    + '<div class="tk-c-t"><b>' + tkEsc(t.name) + '</b>' + b + '</div>'
    + '<div class="tk-c-d">' + tkEsc(t.desc) + '</div>'
    + '<div class="tk-c-f">'
    +   '<span class="tk-c-cmd" title="' + tkEsc(t.cmd) + '">' + tkEsc(t.cmd) + '</span>'
    +   '<button class="tk-c-run" data-tkrun="' + tkEsc(t.id) + '">'
    +     ico('play', 11) + '运行</button>'
    + '</div></div>';
}

/* ── 运行中悬浮条（窗口最小化后，工具还在跑） ───────────────────────────── */
function tkMiniHtml(){
  var r = TK.run;
  if(!r || !r.running || TK.termOpen) return '';
  return '<div class="tk-mini">' + ico('refresh', 14)
    + '<span>正在运行 <b>' + tkEsc(r.name) + '</b>（窗口已最小化，工具还在跑）</span>'
    + '<span class="tk-mini-a">'
    +   '<button class="btn sm" data-tkact="show">' + ico('terminal', 13) + '查看输出</button>'
    +   '<button class="btn sm" data-tkact="stop">停止</button>'
    + '</span></div>';
}

/* ── 终端窗口（弹窗） ───────────────────────────────────────────────────── */
function tkOpenTerm(id){
  var t = tkById(id);
  if(!t) return;
  if(TK.term !== id){ TK.a1 = null; TK.a2 = null; TK.a3 = null; }
  TK.term = id; TK.termOpen = true; TK.cf = null;
  var box = tkEl('modalBox'), mod = tkEl('modal');
  if(!box || !mod){ toast('弹窗容器不在，没法打开终端窗口', 'warn'); return; }
  box.setAttribute('data-term', '1');
  box.innerHTML = tkTermHtml();
  mod.classList.add('open');
  tkFillOut();
}
function tkCloseTerm(){
  TK.termOpen = false;
  var box = tkEl('modalBox');
  if(box){ box.removeAttribute('data-term'); box.innerHTML = ''; }
  if(typeof closeModal === 'function') closeModal();
  tkPaintMini();
}
function tkPaintTerm(){
  if(!TK.termOpen) return;
  tkReadArgs();
  var box = tkEl('modalBox');
  if(!box) return;
  box.innerHTML = tkTermHtml();
  tkFillOut();
}
window.tkOpenTerm = tkOpenTerm;

function tkTermHtml(){
  var t = tkById(TK.term);
  if(!t) return '';
  var run = TK.run, busy = !!(run && run.running);
  var h = '<div class="tk-tm">';

  h += '<div class="tk-tm-h"><div class="tk-tm-ico">' + ico(tkCatIco(t.cat), 18) + '</div>'
    + '<div class="tk-tm-hm">'
    +   '<div class="tk-tm-t">' + tkEsc(t.name)
    +     (t.recommend ? '<span class="chip ok">常用</span>' : '')
    +     (t.long ? '<span class="chip">耗时较长</span>' : '')
    +     (t.backup ? '<span class="chip">自动备份</span>' : '')
    +     (t.reboot ? '<span class="chip warn">需重启生效</span>' : '')
    +   '</div>'
    +   '<div class="tk-tm-c"><code>' + tkEsc(t.cmd) + '</code>'
    +     '<span>超时 ' + (t.timeout || 0) + ' 秒</span></div>'
    + '</div>'
    + '<button class="tk-x" data-tkact="min">最小化</button>'
    + '<button class="tk-x" data-tkact="close" title="关闭窗口（工具继续跑）">关闭</button>'
    + '</div>';

  /* 中间区自己滚：整个窗口不许高过视口（不然顶部的「关闭」会被裁到屏幕外，
     用户点不到，后续操作全打在遮罩上 —— 探针实测踩过）。 */
  h += '<div class="tk-tm-b">';
  h += '<div class="tk-tm-d">' + tkEsc(t.desc) + '</div>';
  if(t.note){
    h += '<div class="tk-note">' + ico('warn', 14) + '<span>' + tkEsc(t.note) + '</span></div>';
  }

  /* 参数区 / 专属面板（跑起来之后不再让人改参数，但保留可见） */
  if(t.argKind === 'netpanel'){ h += tkNetPanelHtml(t); }
  else if(t.argKind === 'speedtest'){ h += tkSpeedHtml(t); }
  else if(t.argKind === 'updatelist'){ h += tkUpdateHtml(t); }
  else { h += tkArgHtml(t); }

  /* 输出 */
  var st = !run ? '<span class="chip">还没跑过</span>'
    : (run.running ? '<span class="chip ok">运行中</span>' : '<span class="chip">已结束</span>');
  var res = (run && run.done) ? ('<span class="tk-o-r ' + (run.code === 0 ? 'ok' : 'warn') + '">返回码 '
    + run.code + ' · 耗时 ' + run.secs + ' 秒' + (run.msg ? (' · ' + tkEsc(run.msg)) : '') + '</span>') : '';
  h += '<div class="tk-tm-o"><div class="tk-o-h">' + ico('terminal', 14) + '<span>输出</span>' + st + res
    + '<span class="tk-o-a">'
    +   (run && !run.running ? '<button class="btn sm" data-tkact="copy">' + ico('copy', 13) + '复制</button>' : '')
    +   (run ? '<button class="btn sm" data-tkact="clear">清空</button>' : '')
    + '</span></div>'
    + '<div class="tk-out" id="tkOut" data-empty="1"></div></div>'
    + '</div>';   /* ← /tk-tm-b 中间区收口 */

  /* 底部：危险工具在这里变成确认条，不再叠第二层弹窗 */
  if(TK.cf && TK.cf.id === t.id){
    h += '<div class="tk-cfbar">' + ico('warn', 15)
      + '<span>这个工具会改动系统（' + tkEsc(t.cmd) + '）'
      + (t.reboot ? '，且需要重启才生效' : '') + '，确认执行？</span>'
      + '<span class="sp"><button class="btn sm" data-tkact="cfno">取消</button>'
      + '<button class="btn sm primary danger" data-tkact="cfyes">确认执行</button></span></div>';
  }else if(busy){
    h += '<div class="tk-tm-f"><span class="tk-act-s">' + ico('refresh', 14) + '正在跑，输出实时更新</span>'
      + '<span class="sp"><button class="btn sm" data-tkact="stop">' + ico('close', 13) + '停止</button>'
      + '<button class="btn sm" data-tkact="min">最小化</button></span></div>';
  }else{
    h += '<div class="tk-tm-f">'
      + '<button class="btn sm primary" data-tkact="run">'
      +   ico('play', 13) + '运行</button>'
      + '<span class="tk-act-h">' + (t.argReq ? '（需要填参数）'
          : (t.argKind ? '（参数可不填，用默认值）' : '（无需参数）')) + '</span>'
      + '<span class="sp"><button class="btn sm" data-tkact="close">关闭</button></span></div>';
  }
  return h + '</div>';
}

/* 通用参数表单：text / drive / opentool / shutdown / 无参数 */
function tkArgHtml(t){
  if(!t.argKind){
    return '<div class="tk-dsec">参数</div><div class="tk-dim2">这个工具不需要参数，直接点「运行」。</div>';
  }
  var h = '<div class="tk-dsec">参数 · ' + tkEsc(t.argLabel || '参数')
    + (t.argReq ? '<span class="tk-req">必填</span>' : '<span class="tk-dim2q">可不填</span>') + '</div>';
  if(t.argKind === 'text'){
    h += '<input class="inp" data-tkarg placeholder="' + tkEsc(t.argHint || t.argLabel || '') + '" value="'
      + tkEsc(TK.a1 == null ? (t.argDef || '') : TK.a1) + '">';
  }else if(t.argKind === 'drive'){
    var cur = TK.a1 == null ? (t.argDef || '') : TK.a1;
    h += '<select class="inp" data-tkarg><option value="">（默认系统盘）</option>'
      + TK.drives.map(function(d){
          return '<option value="' + tkEsc(d) + '"' + (cur === d ? ' selected' : '') + '>' + tkEsc(d) + '</option>';
        }).join('')
      + '</select>';
  }else if(t.argKind === 'opentool'){
    var cv = TK.a1 == null ? (t.argDef || '') : TK.a1;
    h += '<select class="inp" data-tkarg>'
      + (t.opts || []).map(function(o){
          return '<option value="' + tkEsc(o.v) + '"' + (cv === o.v ? ' selected' : '') + '>' + tkEsc(o.t) + '</option>';
        }).join('')
      + '</select>';
  }else if(t.argKind === 'shutdown'){
    h += '<div class="tk-sd"><select class="inp" data-tkact2>'
      +   ['s','r','h'].map(function(v, i){
            var lb = ['关机','重启','睡眠'][i];
            return '<option value="' + v + '"' + ((TK.a2 || 's') === v ? ' selected' : '') + '>' + lb + '</option>';
          }).join('')
      + '</select>'
      + '<input class="inp tk-min" data-tkmin type="number" min="0" step="5" value="'
      +   tkEsc(TK.a3 == null ? '60' : TK.a3) + '">'
      + '<span class="tk-dim2">分钟后</span>'
      + '<button class="btn sm" data-tkact="canceloff">' + ico('close', 13) + '取消关机计划</button></div>';
  }
  return h;
}

/* 网络修复面板：一键 + 单项（子动作名与后端 toolNetSmart 的 switch 对应） */
var TK_NETACTS = [
  {a:'all',     t:'一键智能全修复', d:'清代理 + 刷 DNS + 清 ARP + 对时 + 续租 IP'},
  {a:'proxy',   t:'清除系统代理',   d:'代理被改坏、能上 QQ 打不开网页'},
  {a:'dns',     t:'刷新 DNS 缓存',  d:'域名解析错、换了网络打不开'},
  {a:'arp',     t:'清理 ARP 缓存',  d:'局域网内互访异常'},
  {a:'nic',     t:'续租 IP 地址',   d:'IP 冲突、拿到 169.254 段'},
  {a:'winsock', t:'重置 Winsock',   d:'网络组件损坏，多数需重启'},
  {a:'tcpip',   t:'重置 TCP/IP',    d:'协议栈异常，多数需重启'},
  {a:'time',    t:'同步系统时间',   d:'时间不对导致证书/登录失败'},
  {a:'show',    t:'查看网络配置',   d:'IP / 网关 / DNS 一览'}
];
function tkNetPanelHtml(t){
  var h = '<div class="tk-dsec">一键与单项修复</div><div class="tk-net">'
    + TK_NETACTS.map(function(x){
        return '<button class="tk-net-b' + (x.a === 'all' ? ' all' : '') + '" data-tknet="' + x.a + '">'
          + '<b>' + tkEsc(x.t) + '</b><span>' + tkEsc(x.d) + '</span></button>';
      }).join('')
    + '</div>';
  if(TK.lan === null){
    h += '<div class="tk-dsec">本机局域网信息</div>'
      + '<button class="btn sm" data-tkact="lan">' + ico('wifi', 14) + '查看本机 IP 与共享地址</button>';
  }else if(TK.lan){
    h += '<div class="tk-dsec">本机局域网信息</div><div class="tk-kv">'
      + '<div><span>机器名</span><b>' + tkEsc(TK.lan.hostname || '未获取到') + '</b></div>'
      + '<div><span>本机 IP</span><b>' + tkEsc(TK.lan.ip || '未获取到') + '</b></div>'
      + '<div><span>共享访问</span><b>' + tkEsc(TK.lan.smb || '未获取到') + '</b></div></div>';
  }
  return h;
}

/* 宽带测速：结果直接走窗口里的输出区 */
function tkSpeedHtml(t){
  return '<div class="tk-dsec">测速</div>'
    + '<div class="tk-dim2">点「开始测速」，下载 / 上传 / 延迟实时打在下面的输出区。</div>'
    + '<div class="tk-act"><button class="btn sm primary" data-tkspeed>' + ico('play', 13) + '开始测速</button>'
    + '<span class="tk-act-h">测速会跑几十 MB 流量，别同时下载东西</span></div>';
}

/* 补丁卸载：先列已装补丁，点某条卸载（后端支持 KB 号，自动匹配完整 Identity） */
function tkUpdateHtml(t){
  return '<div class="tk-dsec">已安装的补丁</div>'
    + '<div id="tkUpdBox">' + tkUpdateBoxHtml() + '</div>'
    + '<div class="tk-dsec">手动卸载</div>'
    + '<div class="tk-sd"><input class="inp" data-tkarg placeholder="Package Identity 或 KB 号，例如 KB5034441">'
    + '<button class="btn sm" data-tkact="run">卸载</button></div>'
    + '<div class="tk-dim2">不知道 Identity 就填 KB 号，后端会自动去匹配；匹配到多个时会列出来让你挑。</div>';
}
function tkUpdateBoxHtml(){
  if(TK.updates === null){
    return '<button class="btn sm" data-tkact="updload">' + ico('refresh', 14) + '读取已安装补丁</button>'
      + '<span class="tk-dim2">读取需要几秒（走 Win32_QuickFixEngineering）。</span>';
  }
  if(!TK.updates.length){
    return '<div class="tk-dim2">没读到已安装补丁（精简版系统常见），可以用下面的手动输入。</div>';
  }
  return '<div class="tk-upd"><div class="tk-upd-h"><span>补丁号</span><span>安装日期</span><span></span></div>'
    + TK.updates.map(function(u){
        return '<div class="tk-upd-r">'
          + '<span class="tk-upd-k">' + tkEsc(u.kb || '—') + '</span>'
          + '<span class="tk-upd-d">' + tkEsc(u.date || '—') + '</span>'
          + '<span class="tk-upd-a"><button class="btn sm" data-tkkb="' + tkEsc(u.kb || '') + '">卸载</button></span>'
          + '</div>'
          + (u.desc ? ('<div class="tk-upd-t">' + tkEsc(u.desc) + '</div>') : '');
      }).join('')
    + '</div>';
}

/* 输出填充（整块重画；运行中走增量 append，见 tkOnLog） */
function tkFillOut(){
  var box = tkEl('tkOut');
  if(!box) return;
  var r = TK.run;
  if(!r || !r.lines || !r.lines.length){
    box.dataset.empty = '1';
    box.innerHTML = '<div class="tk-dim2">点「运行」开始，输出会实时滚到这里。</div>';
    return;
  }
  box.dataset.empty = '0';
  box.innerHTML = r.lines.map(function(l){
    return '<div class="tk-l k-' + tkEsc(l.k || 'line') + '">' + tkEsc(l.t) + '</div>';
  }).join('');
  box.scrollTop = box.scrollHeight;
}

/* 待重启提醒条（后端 pendingReboot：跑完标记需重启的，重启后自动消失） */
function tkRebootHtml(){
  if(!TK.pending) return '';
  return '<div class="tk-reboot">' + ico('warn', 15)
    + '<span>有工具标明<b>需要重启才完全生效</b>。重启电脑后这条提醒会自动消失。</span></div>';
}

/* ── 运行 ───────────────────────────────────────────────────────────────── */
function tkReadArgs(){
  var a = document.querySelector('[data-tkarg]');
  if(a) TK.a1 = String(a.value || '');
  var b = document.querySelector('[data-tkact2]');
  if(b) TK.a2 = String(b.value || 's');
  var c = document.querySelector('[data-tkmin]');
  if(c) TK.a3 = String(c.value || '60');
}
function tkArgVal(t){
  if(t.argKind === 'shutdown'){
    var a = document.querySelector('[data-tkact2]');
    var m = document.querySelector('[data-tkmin]');
    var act = a ? a.value : (TK.a2 || 's');
    var min = m ? String(m.value || '').trim() : (TK.a3 == null ? '60' : String(TK.a3));
    if(min === '' || isNaN(Number(min))) min = '60';
    return act + ':' + min;
  }
  var el = document.querySelector('[data-tkarg]');
  return el ? String(el.value || '').trim() : '';
}

function tkRun(id, arg){
  var t = tkById(id);
  if(!t) return;
  if(TK.busy){
    toast('还有工具在跑：' + (TK.run ? TK.run.name : '') + '，先等它结束或点「停止」', 'warn');
    return;
  }
  if(TK.term !== id || !TK.termOpen) tkOpenTerm(id);
  var v = (arg === undefined || arg === null) ? tkArgVal(t) : String(arg);
  if(t.argReq && !v){ toast('这个工具需要填「' + (t.argLabel || '参数') + '」', 'warn'); return; }
  if(t.danger){ TK.cf = {id:id, arg:v}; tkPaintTerm(); return; }
  tkGo(t, v);
}
window.tkRun = tkRun;

function tkGo(t, arg){
  tkReadArgs();
  TK.busy = true; TK.cf = null;
  TK.run = {id:t.id, name:t.name, lines:[], running:true, done:false, code:0, secs:0, msg:''};
  tkPaintTerm(); tkPaintMini();
  callGo('RunTool', t.id, arg || '').then(function(msg){
    if(msg){    /* 非空 = 没启动起来（参数不对 / 已有工具在跑） */
      TK.busy = false;
      TK.run.running = false; TK.run.done = true; TK.run.code = -1; TK.run.msg = msg;
      tkPaintTerm(); tkPaintMini();
      toast(msg, 'danger');
    }
  }).catch(function(e){
    TK.busy = false;
    if(TK.run){ TK.run.running = false; TK.run.done = true; TK.run.code = -1; TK.run.msg = String(e); }
    tkPaintTerm(); tkPaintMini();
    toast('启动失败：' + e, 'danger');
  });
}

/* 事件回调（主文件 onGo('tool:log'/'tool:done') 转发到这里） */
function tkOnLog(kind, text){
  if(!TK.run) TK.run = {id:TK.term || '', name:'工具输出', lines:[], running:true, done:false, code:0, secs:0, msg:''};
  TK.run.lines.push({k:kind || 'line', t:text || ''});
  if(TK.run.lines.length > 3000) TK.run.lines.splice(0, TK.run.lines.length - 3000);
  var box = tkEl('tkOut');
  if(!box) return;    /* 窗口没开着：只记数据，回来 tkFillOut 整块画出来 */
  if(box.dataset.empty === '1'){ box.innerHTML = ''; box.dataset.empty = '0'; }
  var d = document.createElement('div');
  d.className = 'tk-l k-' + (kind || 'line');
  d.textContent = text || '';
  box.appendChild(d);
  box.scrollTop = box.scrollHeight;
}
window.tkOnLog = tkOnLog;

function tkOnDone(code, secs, msg){
  TK.busy = false;
  if(TK.run){
    TK.run.running = false; TK.run.done = true;
    TK.run.code = code; TK.run.secs = secs; TK.run.msg = msg || '';
  }
  tkPaintTerm(); tkPaintMini();
  if(TK.pending === false && tkEl('tkWall')){
    callGo('GetPendingReboot').then(function(st){
      TK.pending = (st === null || st === undefined) ? false
        : (typeof st === 'number' ? st > 0 : !!(st.n > 0 || st.N > 0));
      tkPaintReboot();
    }).catch(function(){});
  }
  toast('工具结束：返回码 ' + code + ' · 耗时 ' + secs + ' 秒' + (msg ? (' · ' + msg) : ''), code === 0 ? 'ok' : 'warn');
}
window.tkOnDone = tkOnDone;

/* ── 交互绑定 ───────────────────────────────────────────────────────────────
   两处容器都要挂：
     #content   —— 卡片墙、分类页签、搜索框、悬浮条（悬浮条是 fixed，仍在 #content 里）
     #modalBox  —— 终端窗口（它在 #content 之外，#content 的委托收不到）
   一律事件委托：innerHTML 换掉后元素会重建，委托才不会失效。 */
function bindTools(){
  var c = tkEl('content');
  if(c && c.dataset.tkBound !== '1'){
    c.dataset.tkBound = '1';
    c.addEventListener('click', tkClick);
  }
  var mb = tkEl('modalBox');
  if(mb && mb.dataset.tkBound !== '1'){
    mb.dataset.tkBound = '1';
    mb.addEventListener('click', tkClick);
  }
  var mod = tkEl('modal');
  if(mod && mod.dataset.tkBound !== '1'){
    mod.dataset.tkBound = '1';
    /* 点遮罩关闭：走 tkCloseTerm，好把 data-term 撤掉（不然下次弹窗会沿用宽版） */
    mod.addEventListener('click', function(e){ if(e.target === mod && TK.termOpen) tkCloseTerm(); });
  }
  /* 进页补加载：只靠启动那次预热不够（预热时后端绑定可能还没注入，等于空跑）。
     数据没到就再拉一次 —— 别的模块都是这么兜的，工具库之前漏了。 */
  if(TK.state !== 'ready' && !TK.loading) tkLoad();
  /* 症状清单单独拉（跟工具目录不是一个后端方法），进页补一次 */
  if(TK.guideState !== 'ready' && !TK.guideLoading) tkLoadGuides();
}
window.bindTools = bindTools;

function tkHandle(ds){
  /* ── 对症排查 ── */
  if('flfold' in ds){ TK.guideFold = !TK.guideFold; tkPaintFlow(); return true; }
  if(ds.fltog){ TK.guideOpen[ds.fltog] = !TK.guideOpen[ds.fltog]; tkPaintFlow(); return true; }
  if(ds.flact){
    for(var fi = 0; fi < TK.guides.length; fi++){ if(TK.guides[fi].id === ds.flact){ tkFlowAct(TK.guides[fi]); break; } }
    return true;
  }
  if('flart' in ds){
    callGo('CopyText', ds.flart).then(function(m){
      toast(m ? ('复制失败：' + m) : ('文章链接已复制：' + (ds.arttitle || '')), m ? 'warn' : 'ok');
    }).catch(function(err){ toast('复制失败：' + err, 'warn'); });
    return true;
  }
  if('flretry' in ds){ TK.guideLoading = false; tkLoadGuides(); return true; }
  if(ds.tkcat){
    TK.cat = ds.tkcat;
    tkPaintTabs(); tkPaintWall();
    return true;
  }
  if(ds.tkopen){ tkOpenTerm(ds.tkopen); return true; }
  if(ds.tkrun){
    var tr = tkById(ds.tkrun);
    if(tr){ if(tkAutoRun(tr)) tkRun(ds.tkrun); else tkOpenTerm(ds.tkrun); }
    return true;
  }
  if(ds.tknet){ tkRun('net-smart', ds.tknet); return true; }
  if('tkspeed' in ds){ tkRun('net-speed', ''); return true; }
  if('tkkb' in ds){
    if(!ds.tkkb){ toast('这条补丁没有 KB 号，请用下面的手动输入', 'warn'); return true; }
    tkRun('fix-update-remove', ds.tkkb);
    return true;
  }
  if(ds.tkact){
    var a = ds.tkact;
    if(a === 'retry' || a === 'rerun'){ TK.state = 'loading'; TK.err = ''; if(typeof render === 'function') render(); tkLoad(); return true; }
    if(a === 'run'){ if(TK.term) tkRun(TK.term); return true; }
    if(a === 'cfyes'){
      var cf = TK.cf; TK.cf = null;
      if(cf){ var tc = tkById(cf.id); if(tc) tkGo(tc, cf.arg); }
      return true;
    }
    if(a === 'cfno'){ TK.cf = null; tkPaintTerm(); return true; }
    if(a === 'stop'){ callGo('StopTool').then(function(){ toast('已请求停止当前工具', 'info'); }).catch(function(){}); return true; }
    if(a === 'clear'){ TK.run = null; tkPaintTerm(); return true; }
    if(a === 'copy'){
      if(!TK.run || !TK.run.lines.length){ toast('没有可复制的输出', 'warn'); return true; }
      var txt = TK.run.lines.map(function(l){ return l.t; }).join('\n');
      callGo('CopyText', txt).then(function(m){ toast(m ? ('复制失败：' + m) : '输出已复制', m ? 'warn' : 'ok'); })
        .catch(function(err){ toast('复制失败：' + err, 'warn'); });
      return true;
    }
    if(a === 'min' || a === 'close'){ tkCloseTerm(); return true; }
    if(a === 'show'){ tkOpenTerm(TK.run ? TK.run.id : TK.term); return true; }
    if(a === 'canceloff'){ tkRun('util-shutdown-cancel', ''); return true; }
    if(a === 'updload'){ tkLoadUpdates(); return true; }
    if(a === 'lan'){
      callGo('GetLanShareInfo').then(function(m){ TK.lan = m || {}; tkPaintTerm(); })
        .catch(function(){ TK.lan = {}; tkPaintTerm(); });
      return true;
    }
  }
  return false;
}

function tkClick(e){
  var root = e.currentTarget;
  var el = e.target;
  while(el && el !== root){
    if(el.dataset && tkHandle(el.dataset)){ e.stopPropagation(); return; }
    el = el.parentNode;
  }
}

function tkLoadUpdates(){
  var box = tkEl('tkUpdBox');
  if(!box) return;
  box.innerHTML = '<div class="tk-dim2">正在读取已安装补丁…</div>';
  callGo('GetInstalledUpdates').then(function(list){
    TK.updates = list || [];
    var b = tkEl('tkUpdBox');
    if(b) b.innerHTML = tkUpdateBoxHtml();
  }).catch(function(e){
    TK.updates = [];
    var b2 = tkEl('tkUpdBox');
    if(b2) b2.innerHTML = '<div class="tk-dim2">读取失败：' + tkEsc(e) + '</div>';
  });
}

/* Ctrl+K 命令面板：跳到工具库并打开该工具的终端窗口（能自动跑的直接跑） */
function tkJump(id){
  var t = tkById(id);
  if(!t){ if(typeof go === 'function') go('tools'); return; }
  TK.cat = tkSceneOf(t.id);
  if(!tkCnt(TK.cat)) TK.cat = 'all';
  if(typeof go === 'function') go('tools');
  tkOpenTerm(id);
  if(tkAutoRun(t)) setTimeout(function(){ tkRun(id); }, 160);
}
window.tkJump = tkJump;
window.tkForceLoad = tkLoad;
