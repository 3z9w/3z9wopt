/* ══════════════════════════════════════════════════════════════════════════
   应用卸载页 · 独立模块（design/uninst.js + design/uninst.css）
   ---------------------------------------------------------------------------
   2026-10-11 重做。旧版（2026-10-09）那页是「卡片列表 + 5 个筛选页签 + 写死的盒子图标」，
   这一版改成**控制面板式的表格**：图标 / 名称 / 发布者 / 版本 / 安装时间 / 大小 六列，
   点表头即可排序；顶部两个 Tab 分开「桌面程序」与「系统内置应用（UWP）」。

   载入方式见 <head> 里的「应用卸载页独立文件加载器」：同步 XHR + 直接 eval，
   保证 window.renderAppx / window.loadApps / window.bindUninst 在 PAGES 引用它们之前就位。
   这个 IIFE 的作用域是"模块内"的：主文件要访问的都显式挂 window（APPS / renderAppx /
   loadApps / bindUninst），状态变量用 var 留在模块内即可。

   数据全部真读，不编造（项目中红线）：
     · 列表   GetWin32Apps + GetUwpApps（后端枚举注册表卸载项 / Appx 清单）
     · 图标   GetAppIcon(iconKey) —— **列表先出、图标后填**，后端带磁盘缓存，第二次秒开
     · 体积   bytes + sizeSrc（dir 实测 / reg 申报 / none 未知）；未知就是未知，不填 0
     · 卸载   UninstallApp(id) 调**软件自己的卸载程序**；之后用 PollUninstall 盯它的进程，
              进程一退出才重新枚举确认它真从列表消失。
              ⚠️ **不显示百分比**：软件自己的进度我们从外部读不到，画出来就是编的
              （2026-10-11 用户卸 UU 加速器时发现"两边进度对不上"，原写法是按项数算的假进度）。
   ══════════════════════════════════════════════════════════════════════════ */

/* 数据与对外入口挂 window：主文件 PAGES / bindPage / 初始化预读都引用它们 */
window.APPS = window.APPS || [];
window.UN_SIZE = window.UN_SIZE || { running: false, done: 0, total: 0, stop: false };

/* 模块内状态（不挂 window，主文件不需要） */
var unState = 'loading', unErr = '';
var unTab = 'win32';                 // win32 | uwp —— 顶部两个 Tab
var unQuery = '';
var unSort = 'bytes', unAsc = false; // 默认按体积从大到小
var UN_BUSY = false, UN_MARK = {};
var UN_RESULT = [];                  // 卸载结果快照 {name,bytes,gone}
var UN_DONE = false;                 // 是否正停在结果页（决定刷新列表时能不能覆盖页面）
var ICON_MEM = {};                   // 图标内存缓存（避免重绘后重复问后端）

/* 卸载 → 查残留 → 清理（2026-10-11 用户定的流程） */
var UN_CUR = null;                   // 当前这一个：{id,name,bytes,uwp}
var UN_RES = [];                     // 扫到的残留（后端 ResidueItem，带 safe/guard）
var UN_RES_ON = {};                  // 残留勾选：id → true
var UN_BAK = '';                     // 注册表备份目录（后端 ResidueBackupDir）
var UN_SUM = null;                   // 完成页汇总 {name,bytes,gone,resN,cleaned,freed,fails,skipped}

/* ── 小工具 ─────────────────────────────────────────────────────────────── */
function unEsc(s){
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
/* 体积：小于 1 GB 用 MB，0 / 未知一律显示「—」（不许编 0） */
function unSize(b){
  b = b || 0;
  if(!b) return '—';
  if(b >= 1024 * 1024 * 1024) return (b / 1073741824).toFixed(2) + ' GB';
  return Math.round(b / 1048576) + ' MB';
}
function unSrcLabel(a){
  return a.sizeSrc === 'dir' ? '实测' : (a.sizeSrc === 'reg' ? '申报' : '未知');
}
function unDate(a){
  if(!a.date) return '—';
  var d = new Date(a.date * 1000);
  return d.getFullYear() + '-' + ('0' + (d.getMonth() + 1)).slice(-2) + '-' + ('0' + d.getDate()).slice(-2);
}
/* 2026-10-09 修过的一条：kind === 'uwp' 才算内置（后端从来没把 sys 置 true，
   按 sys 判会恒为 0）。保留 sys 兼容分支，旧后端上线也不会是 0。 */
function isUwp(a){ return !!a && (a.kind === 'uwp' || a.sys === true); }
function unCanRemove(a){
  return !!(a && a.removable !== false && !a.blocked);
}
function unSel(){ return APPS.filter(function(a){ return a.on; }); }
function unCounts(){
  var win = 0, uwp = 0, bytes = 0, dir = 0, reg = 0;
  APPS.forEach(function(a){
    if(isUwp(a)) uwp++; else win++;
    bytes += (a.bytes || 0);
    if(a.sizeSrc === 'dir') dir++; else if(a.sizeSrc === 'reg') reg++;
  });
  return { total: APPS.length, win: win, uwp: uwp, bytes: bytes, dir: dir, reg: reg };
}
function unSizeNeed(){
  return APPS.filter(function(a){ return a.sizePending && a.installLoc; }).length;
}

/* ── 读取 ───────────────────────────────────────────────────────────────── */
/* keepDone = true 表示这次刷新是「卸载收尾时顺带刷新真实列表」，结果页要留住；
   常规刷新（进页 / 点重新枚举）则作废结果页，正常回到列表。 */
function loadApps(keepDone){
  if(!hasGo()){
    if(inWails()){ unState = 'failed'; unErr = 'window.go 没拿到（后端未连接）'; }
    else { unState = 'demo'; }
    return;
  }
  if(!keepDone) UN_DONE = false;
  /* hold = 界面正停在结果页：这轮刷新**只更新数据、不许碰界面** ——
     开头那句 unState='loading' + render() 会先画一屏骨架，把结果页整个冲掉
     （2026-10-11 实测：卸完只剩"正在枚举已安装的软件…"，结果一闪而过）。 */
  var hold = UN_DONE;
  if(!hold){
    unState = 'loading';
    if(typeof render === 'function' && cur === 'appx') render();
  }
  Promise.all([callGo('GetWin32Apps'), callGo('GetUwpApps')]).then(function(r){
    APPS = (r[0] || []).concat(r[1] || []);
    if(hold){
      /* 结果页用 UN_RESULT 快照渲染，新数据等用户点「返回列表」再用。
         图标先取回内存，回去时就是现成的。 */
      setTimeout(unPrefetchIcons, 60);
      return;
    }
    unState = 'ready';
    if(cur === 'appx') render();
    /* 图标后填：主文件里的 render() 是函数声明、调用不经过 window.render 包装
       （见文件末尾），所以这里显式补一次，别指望包装兜住。 */
    setTimeout(unFillIcons, 0);
    /* 后台把**两个 Tab 的图标都取回来**（2026-10-11 修：原来只填当前 Tab，
       切到「系统内置应用」时那一栏还是一串灰盒子，要等它重新排队才慢慢冒出来）。 */
    setTimeout(unPrefetchIcons, 60);
    if(typeof syncHomeLive === 'function' && cur === 'home') syncHomeLive();
  }).catch(function(e){
    if(hold) return;   // 结果页期间读失败也别打扰，用户看到的结果已经定了
    unState = 'failed'; unErr = String(e);
    if(cur === 'appx') render();
  });
}
/* ⚠️ 必须挂 window：主文件 appx 页进页 / reloadCurrentPage 调的是全局 loadApps()，
   模块内的 function 声明不出这个 IIFE 的作用域 —— 漏了这行，主文件就落到加载器里
   那个空兜底函数上，页面永远停在骨架屏（2026-10-11 的实际事故）。 */
window.loadApps = loadApps;

/* ── 排序 / 过滤 ────────────────────────────────────────────────────────── */
var UN_COLS = [
  { k: 'name', t: '名称', cls: '' },
  { k: 'publisher', t: '发布者', cls: 'un-col-pub' },
  { k: 'version', t: '版本', cls: 'un-col-ver' },
  { k: 'date', t: '安装时间', cls: '' },
  { k: 'bytes', t: '大小', cls: '' }
];
function unSorted(){
  var l = APPS.filter(function(a){ return unTab === 'uwp' ? isUwp(a) : !isUwp(a); });
  if(unQuery){
    var q = unQuery.toLowerCase();
    l = l.filter(function(a){
      return ((a.name || '') + ' ' + (a.publisher || '') + ' ' + (a.version || '')).toLowerCase().indexOf(q) >= 0;
    });
  }
  var f = unSort, dir = unAsc ? 1 : -1;
  l.sort(function(x, y){
    var vx, vy;
    if(f === 'bytes'){ vx = x.bytes || 0; vy = y.bytes || 0; }
    else if(f === 'date'){ vx = x.date || 0; vy = y.date || 0; }
    else { vx = String(x[f] || '').toLowerCase(); vy = String(y[f] || '').toLowerCase(); }
    if(vx < vy) return -dir;
    if(vx > vy) return dir;
    return String(x.name || '').localeCompare(String(y.name || ''), 'zh');
  });
  return l;
}

/* ── 渲染 ───────────────────────────────────────────────────────────────── */
function unHeadHtml(){
  var th = '<div class="un-th un-cols">';
  /* 第一列现在是空白占位：2026-10-11 用户定「不给批量卸载」，一次只勾一个，
     所以没有"表头全选"这种东西了（列还在，保证表头与数据行对齐）。 */
  th += '<div></div>';
  th += '<div></div>'; // 图标列（不可排序，占位保证列对齐）
  UN_COLS.forEach(function(c){
    var on = unSort === c.k;
    th += '<div class="' + c.cls + '"><button class="un-th-i' + (on ? ' on' : '')
        + (on && unAsc ? ' asc' : '') + '" data-unsort="' + c.k + '">' + c.t
        + '<span class="un-arw">▼</span></button></div>';
  });
  th += '<div></div>'; // 操作列
  return th + '</div>';
}
function unRowHtml(a){
  var idx = APPS.indexOf(a);
  var can = unCanRemove(a);
  var cached = ICON_MEM[a.iconKey];
  return '<div class="un-row un-cols' + (a.on ? ' sel' : '') + (can ? '' : ' no') + '" data-unrow="' + idx + '">'
    + '<div class="cbx' + (a.on ? ' on' : '') + '" data-unpick="' + idx + '">' + (a.on ? ico('check', 12) : '') + '</div>'
    + '<div class="un-ico' + (a.hasIcon && !cached ? ' pend' : '') + '" data-ic="' + unEsc(a.hasIcon ? a.iconKey : '') + '">'
    +   (cached ? '<img src="' + cached + '" alt="">' : ico('box', 15))
    + '</div>'
    + '<div class="un-nm"><span>' + unEsc(a.name)
    +   (a.neverRan ? ' <span class="chip warn">未运行</span>' : '')
    +   (can ? '' : ' <span class="chip">不可卸载</span>')
    + '</span></div>'
    + '<div class="un-dim un-col-pub" title="' + unEsc(a.publisher) + '">' + unEsc(a.publisher || '—') + '</div>'
    + '<div class="un-dim un-col-ver">' + unEsc(a.version || '—') + '</div>'
    + '<div class="un-dim">' + unDate(a) + '</div>'
    + '<div class="un-num">' + ((a.bytes || 0) ? '<b>' + unSize(a.bytes) + '</b>' : '—')
    +   ' <span class="chip">' + unSrcLabel(a) + '</span></div>'
    + '<div class="un-op"><button class="un-mini" data-unone="' + idx + '"'
    +   (can ? '' : ' disabled') + '>卸载</button></div>'
    + '</div>';
}
function unSkeleton(){
  var s = '';
  for(var i = 0; i < 6; i++){
    s += '<div class="un-sk-row un-cols">'
      + '<div class="un-sk" style="width:16px;height:16px;border-radius:5px"></div>'
      + '<div class="un-sk" style="width:26px;height:26px;border-radius:7px"></div>'
      + '<div class="un-sk" style="width:' + (40 + (i * 9) % 45) + '%"></div>'
      + '<div class="un-sk un-col-pub" style="width:70%"></div>'
      + '<div class="un-sk un-col-ver" style="width:60%"></div>'
      + '<div class="un-sk" style="width:70%"></div>'
      + '<div class="un-sk" style="width:60%;margin-left:auto"></div>'
      + '<div></div></div>';
  }
  return s;
}
window.renderAppx = function(){
  if(unState === 'run')     return unRenderRun();
  if(unState === 'scan')    return unRenderScan();
  if(unState === 'residue') return unRenderResidue();
  if(unState === 'clean')   return unRenderClean();
  if(unState === 'done')    return unRenderDone();

  var head0 = '<div class="page-head" style="margin-bottom:12px"><div class="page-title">应用卸载</div>'
    + '<div class="page-sub">' + (unState === 'ready'
        ? '共 ' + APPS.length + ' 个（桌面 ' + unCounts().win + ' · 系统内置 ' + unCounts().uwp + '）。'
          + '卸载走<b>软件自己的卸载程序</b>，卸完靠<b>重新枚举</b>确认，不假装进度条。'
        : '正在枚举已安装的软件…') + '</div></div>';

  if(unState === 'failed'){
    return head0
      + '<div class="card"><div class="empty" style="padding:48px 20px">'
      + '<div class="empty-ico" style="color:var(--danger)">' + ico('warn', 28) + '</div>'
      + '<div class="empty-t">没能读到已安装软件</div><div class="empty-d">' + unEsc(unErr) + '</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      + '<button class="btn primary" id="unRetry">' + ico('refresh', 16) + '重新枚举</button></div></div></div>';
  }
  if(unState !== 'ready'){
    /* 首次读取会慢（枚举 + 体积），给骨架屏 —— 不是转圈，让用户直接看到"表格要来了" */
    return head0
      + '<div class="un-tbl">' + unHeadHtml() + unSkeleton() + '</div>'
      + '<div class="un-sp"></div>';
  }

  var c = unCounts(), list = unSorted(), sel = unSel();
  var selBytes = sel.reduce(function(s, a){ return s + (a.bytes || 0); }, 0);

  var hero = '<div class="un-hero">'
    + '<div class="un-fig">' + ico('grid', 22) + '</div>'
    + '<div class="un-kpis">'
    +   '<div class="un-kpi"><b>' + c.total + '</b><span>已安装</span></div>'
    +   '<div class="un-kpi"><b>' + c.win + '</b><span>桌面程序</span></div>'
    +   '<div class="un-kpi"><b>' + c.uwp + '</b><span>系统内置</span></div>'
    +   '<div class="un-kpi"><b style="color:var(--ok)">' + unSize(c.bytes) + '</b><span>已统计体积</span></div>'
    + '</div>'
    + '<div class="un-hero-r">'
    +   '<span class="chip">实测 ' + c.dir + '</span>'
    +   '<span class="chip">申报 ' + c.reg + '</span>'
    + '</div></div>';

  var tabs = '<div class="un-tabs">'
    + '<button class="un-tab' + (unTab === 'win32' ? ' on' : '') + '" data-untab="win32">桌面程序 <b>' + c.win + '</b></button>'
    + '<button class="un-tab' + (unTab === 'uwp' ? ' on' : '') + '" data-untab="uwp">系统内置应用 <b>' + c.uwp + '</b></button>'
    + '</div>';

  var search = '<label class="un-search">'
    + '<svg width="14" height="14" viewBox="0 0 14 14" fill="none" aria-hidden="true">'
    + '<circle cx="6" cy="6" r="4.2" stroke="currentColor" stroke-width="1.5"/>'
    + '<path d="M9.3 9.3 12.3 12.3" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>'
    + '<input id="unQ" type="text" autocomplete="off" spellcheck="false"'
    + ' placeholder="搜索：名称 / 发布者 / 版本号…" value="' + unEsc(unQuery) + '"></label>';

  var tbl = '<div class="un-tbl">' + unHeadHtml()
    + (list.length ? list.map(unRowHtml).join('')
        : '<div class="empty" style="padding:34px 20px"><div class="empty-t">'
          + (unQuery ? '没有匹配的软件' : '这一类里没有软件')
          + '</div><div class="empty-d">'
          + (unQuery ? '换个词试试，或清空搜索框。' : '切到另一个页签看看。')
          + '</div></div>')
    + '</div>';

  /* 一次只卸一个：底栏直接把选中的名字写出来，按钮就叫「卸载」。 */
  var one = sel.length ? sel[0] : null;
  var bar = '<div class="un-bar">'
    + '<span class="un-bar-n">'
    +   (one ? '已选 <b>' + unEsc(one.name) + '</b> · ' + unSize(one.bytes || 0)
             : '<span style="color:var(--tx3)">勾一个再卸载 —— 一次只卸一个，卸完自动查残留</span>')
    + '</span>'
    + '<button class="btn sm" id="unOpenDir"' + (one && one.installLoc ? '' : ' disabled') + '>'
    +   ico('folder', 14) + '打开目录</button>'
    + (unSizeNeed() ? '<button class="btn sm" id="unSizeAll">' + ico('box', 14) + '实测体积（' + unSizeNeed() + ' 个）</button>' : '')
    + '<span class="un-bar-sp"></span>'
    + '<button class="btn sm" id="unClear"' + (one ? '' : ' disabled') + '>取消选择</button>'
    + '<button class="btn sm primary" id="unGo"' + ((!one || UN_BUSY) ? ' disabled' : '') + '>'
    +   ico('trash', 14) + '卸载</button>'
    + '</div>';

  return head0 + hero + tabs + search + tbl + '<div class="un-sp"></div>' + bar;
};

/* ── 卸载监视 / 结果 ─────────────────────────────────────────────────────────
   2026-10-11 重做：原来是「进度条 + 百分比」，但那个百分比是**按项数算的**
   （第 1/3 个 = 33%），跟软件自己窗口里的 20%、60% 毫无关系 —— 用户卸 UU 加速器时
   看到的正是"两边进度对不上"。软件的真实进度我们从外部**读不到**，画出来就是编的。

   所以这一版只显示**观测得到的事实**：
     · 卸载进程还在不在跑（后端 PollUninstall 的 procN，按进程名实测）
     · 它退出后重新枚举，这一项是不是真从系统列表里消失了
     · 已经等了多少秒（这个是真的）
   没有百分比，没有自己往下走的进度条。                                        */
function unRenderRun(){
  var it = UN_CUR || { name: '…', bytes: 0 };
  return '<div class="page-head"><div class="page-title">正在卸载</div>'
    + '<div class="page-sub">调用软件<b>自己的卸载程序</b>，进度看它弹出的窗口 —— 我只盯着它跑完，'
    + '再重新枚举确认它真的从系统里消失，然后自动扫一遍它留下的注册表残留。'
    + '下面是<b>实测状态</b>，不是百分比。</div></div>'
    + '<div class="un-run">'
    +   '<div class="un-run-h"><div class="scan-ring" style="width:22px;height:22px"></div>'
    +     '<b id="unNow">' + unEsc(it.name) + '</b><em id="unTick">准备中…</em></div>'
    +   '<div class="un-run-wait"><span id="unSec">已等待 0 秒</span>'
    +     '<button class="btn sm" id="unStop">结束等待，看结果</button></div>'
    + '</div>'
    + '<div class="prog-list un-prog">'
    +   '<div class="prog-item wait" id="uni0">'
    +     '<span class="un-pr-ico" id="upi0" style="color:var(--tx3)">' + ico('box', 16) + '</span>'
    +     '<div class="pn">' + unEsc(it.name) + ' <span style="color:var(--tx3)">· ' + unSize(it.bytes) + '</span></div>'
    +     '<div class="ps" id="uns0">等待中</div></div>'
    + '</div>';
}
/* 卸载程序已退出、且确认从系统列表消失 → 自动进残留对比（这一屏很短，说清在干嘛即可） */
function unRenderScan(){
  return '<div class="page-head"><div class="page-title">已卸载，正在查残留</div>'
    + '<div class="page-sub">拿卸载前记下的落点跟现在逐条对比，还在的就是残留。</div></div>'
    + '<div class="un-run">'
    +   '<div class="un-run-h"><div class="scan-ring" style="width:22px;height:22px"></div>'
    +     '<b>' + unEsc((UN_CUR && UN_CUR.name) || '') + '</b><em>正在对比…</em></div>'
    + '</div>';
}
function unRenderClean(){
  return '<div class="page-head"><div class="page-title">正在清理残留</div>'
    + '<div class="page-sub">一条一条删，删前先导出备份。</div></div>'
    + '<div class="un-run"><div class="un-run-h">'
    +   '<div class="scan-ring" style="width:22px;height:22px"></div><b>清理中…</b></div></div>';
}

/* ── 残留清单 ───────────────────────────────────────────────────────────────
   默认勾选 = 后端 safe=true（Geek 口径：命中程序名/发布者/GUID/包名，且不在系统区）。
   safe=false 的行标 ⚠ 写明原因、默认不勾、点也不让勾（后端同样会拒绝）。       */
function unResSel(){ return UN_RES.filter(function(r){ return !!UN_RES_ON[r.id]; }); }
function unResRow(r){
  var on = !!UN_RES_ON[r.id];
  return '<div class="un-res' + (on ? ' on' : '') + (r.safe ? '' : ' lock') + '" data-unres="' + unEsc(r.id) + '">'
    + '<div class="cbx' + (on ? ' on' : '') + '">' + (on ? ico('check', 12) : '') + '</div>'
    + '<div class="un-res-m">'
    +   '<div class="un-res-p">' + unEsc(r.path)
    +     (r.val ? ' <span class="chip">值：' + unEsc(r.val) + '</span>' : '') + '</div>'
    +   '<div class="un-res-d">'
    +     (r.safe ? unEsc(r.reason || '')
                  : ico('warn', 11) + ' ' + unEsc(r.guard || '判不准，我不自动清')) + '</div>'
    + '</div>'
    + '<div class="un-res-t">'
    +   (r.kind === 'dir' ? unSize(r.bytes) : (r.val ? '注册表值' : '注册表项')) + '</div>'
    + '</div>';
}
function unRenderResidue(){
  var regs = UN_RES.filter(function(r){ return r.kind === 'reg'; });
  var dirs = UN_RES.filter(function(r){ return r.kind === 'dir'; });
  var sel = unResSel();
  var bytes = sel.reduce(function(s, r){ return s + (r.bytes || 0); }, 0);
  var safe = UN_RES.filter(function(r){ return r.safe; });
  var allOn = safe.length && safe.every(function(r){ return !!UN_RES_ON[r.id]; });

  var s = '<div class="page-head"><div class="page-title">卸载完成 · 发现 ' + UN_RES.length + ' 项残留</div>'
    + '<div class="page-sub">' + unEsc((UN_CUR && UN_CUR.name) || '') + ' 已经不在系统列表里了。'
    + '下面是它留下的痕迹：<b>默认勾的是确定属于它的</b>（按程序名 / 发布者 / 安装包编号比对，'
    + '和 GeekUninstaller 同一套口径）；没勾的不是判不准就是在系统区，我不动。</div></div>'
    + '<div class="un-res-top">' + ico('shield', 15)
    +   '<span>删之前每条注册表项都会<b>先导出备份</b>（' + unEsc(UN_BAK || '临时目录') + '），'
    +     '真删错了双击那个 .reg 就能导回来。文件夹默认不勾 —— 文件删了没法还原。</span></div>';
  if(regs.length){
    s += '<div class="un-res-h">注册表残留 <b>' + regs.length + '</b></div>'
      + '<div class="un-tbl un-res-tbl">' + regs.map(unResRow).join('') + '</div>';
  }
  if(dirs.length){
    s += '<div class="un-res-h">文件夹残留 <b>' + dirs.length + '</b></div>'
      + '<div class="un-tbl un-res-tbl">' + dirs.map(unResRow).join('') + '</div>';
  }
  s += '<div class="un-sp"></div>'
    + '<div class="un-bar">'
    +   '<span class="un-bar-n">已选 <b id="unRN">' + sel.length + '</b> 项'
    +     (bytes ? ' · 可释放 <b style="color:var(--ok)">' + unSize(bytes) + '</b>' : '') + '</span>'
    +   '<button class="btn sm" id="unResAll">' + (allOn ? '全不选' : '全选（确定属于它的）') + '</button>'
    +   '<span class="un-bar-sp"></span>'
    +   '<button class="btn sm" id="unCleanSkip">不清理，直接完成</button>'
    +   '<button class="btn sm primary" id="unCleanGo"' + (sel.length ? '' : ' disabled') + '>'
    +     ico('trash', 14) + '清理所选</button>'
    + '</div>';
  return s;
}
function unRenderDone(){
  var s = UN_SUM || { name: '', bytes: 0, gone: false, resN: 0, cleaned: 0, freed: 0, fails: [], skipped: false };
  var card = '<div class="card" style="padding:20px;margin-bottom:14px;text-align:center">'
    + '<div style="width:52px;height:52px;border-radius:50%;margin:0 auto 13px;display:grid;place-items:center;'
    +   'background:' + (s.gone ? 'rgba(52,211,153,.14);border:1px solid rgba(52,211,153,.32);color:var(--ok)'
                               : 'rgba(251,191,36,.14);border:1px solid rgba(251,191,36,.32);color:var(--warn)')
    +   '">' + ico(s.gone ? 'check' : 'warn', 26) + '</div>'
    + '<div style="font-size:15px;font-weight:640;margin-bottom:6px">' + unEsc(s.name) + ' · '
    +   (s.gone ? '已确认卸载' : '没确认（可能还在系统卸载窗口里等你点）') + '</div>'
    + '<div style="font-size:12.5px;color:var(--tx3)">体积 ' + unSize(s.bytes) + '</div></div>';

  var res = '';
  if(s.uwp){
    /* 内置应用：用户定「不用清注册表」—— 它们的注册项由 Windows 自己管，动不得 */
    res = '<div class="card" style="padding:16px 18px;margin-bottom:14px">'
      + '<div style="font-weight:600;margin-bottom:5px">已移除，不清理注册表</div>'
      + '<div style="font-size:12.5px;color:var(--tx3)">系统内置应用的注册项由 Windows 自己管，'
      +   '动它容易出问题，这一步跳过。</div></div>';
  } else if(!s.resN){
    res = '<div class="card" style="padding:16px 18px;margin-bottom:14px">'
      + '<div style="font-weight:600;margin-bottom:5px">没有发现残留</div>'
      + '<div style="font-size:12.5px;color:var(--tx3)">卸载前后对比过了，它没留下什么，不用清。</div></div>';
  } else if(s.skipped){
    res = '<div class="card" style="padding:16px 18px;margin-bottom:14px">'
      + '<div style="font-weight:600;margin-bottom:5px">残留 ' + s.resN + ' 项，你选择不清理</div>'
      + '<div style="font-size:12.5px;color:var(--tx3)">一个都没动。</div></div>';
  } else {
    res = '<div class="card" style="padding:16px 18px;margin-bottom:14px">'
      + '<div style="font-weight:600;margin-bottom:5px">清理了 ' + s.cleaned + ' / ' + s.resN + ' 项残留'
      +   (s.freed ? '，释放 ' + unSize(s.freed) : '') + '</div>'
      + '<div style="font-size:12.5px;color:var(--tx3)">删掉的注册表项都留了 .reg 备份：'
      +   unEsc(UN_BAK || '临时目录') + '</div></div>';
    if(s.fails && s.fails.length){
      res += '<div class="card" style="padding:16px 18px;margin-bottom:14px">'
        + '<div style="font-weight:600;margin-bottom:8px;color:var(--warn)">' + s.fails.length + ' 项没能清掉</div>'
        + '<div class="rows">' + s.fails.map(function(f){
            return '<div class="row"><span style="color:var(--warn);flex:0 0 16px">' + ico('warn', 16) + '</span>'
              + '<div class="grow"><div class="row-t" style="font-size:12px;word-break:break-all">'
              +   unEsc(f.path) + '</div>'
              + '<div class="row-d">' + unEsc(f.err) + '</div></div></div>';
          }).join('') + '</div></div>';
    }
  }
  return '<div class="page-head"><div class="page-title">卸载流程结束</div>'
    + '<div class="page-sub">列表已经刷新过，回去就是最新的。</div></div>'
    + card + res
    + '<div class="btn-row"><button class="btn primary" id="unBack">返回列表</button></div>';
}

/* ── 图标：列表先出，图标后填（带内存缓存）───────────────────────────────── */
var UN_ICON_SEQ = 0;

/* 后台预取全部图标（不分 Tab）。第一次进页时 UWP 图标要去 WindowsApps 目录里
   现读 AppxManifest + PNG，逐个排队才轮得到；先取回来，切页签时就是秒出。 */
async function unPrefetchIcons(){
  var keys = [];
  APPS.forEach(function(a){
    if(a.hasIcon && a.iconKey && !ICON_MEM[a.iconKey] && keys.indexOf(a.iconKey) < 0) keys.push(a.iconKey);
  });
  var i = 0;
  async function worker(){
    for(;;){
      if(i >= keys.length) return;
      var k = keys[i++];
      try{
        var url = await callGo('GetAppIcon', k);
        if(url) ICON_MEM[k] = url;
      }catch(e){ /* 取不到就留占位，不拖累别的 */ }
    }
  }
  var ws = []; for(var n = 0; n < 3; n++) ws.push(worker());
  await Promise.all(ws);
  unFillIcons(); // 当前页里还空着的补上
}

async function unFillIcons(){
  var els = [].slice.call(document.querySelectorAll('[data-ic]'));
  var todo = els.filter(function(el){ return el.getAttribute('data-ic') && !el.getAttribute('data-done'); });
  if(!todo.length) return;
  var i = 0;
  async function worker(){
    for(;;){
      if(i >= todo.length) return;
      var el = todo[i++];
      var key = el.getAttribute('data-ic');
      el.setAttribute('data-done', '1');
      try{
        var url = ICON_MEM[key];
        if(!url){
          url = await callGo('GetAppIcon', key);
          if(url) ICON_MEM[key] = url;
        }
        if(url) el.innerHTML = '<img src="' + url + '" alt="">';
      }catch(e){ /* 取不到就留占位图标，不影响列表 */ }
      el.classList.remove('pend');
    }
  }
  var ws = []; for(var k = 0; k < 6; k++) ws.push(worker());
  await Promise.all(ws);
}

/* ── 交互 ───────────────────────────────────────────────────────────────── */
/* 一次只勾一个（2026-10-11 用户定：不给批量卸载）。
   技术原因也在这儿：后端 appSess 是单会话，卸第二个会把第一个的"卸载前快照"顶掉，
   残留就比不出来了 —— 所以只能一个一个来。 */
function unPick(idx, on){
  var a = APPS[idx];
  if(!a || !unCanRemove(a)) return;
  APPS.forEach(function(x){ x.on = false; });
  a.on = !!on;
  unSyncSel();
}
/* 只改底栏文字与行样式，不整页重绘（重绘会吞点击，项目铁律） */
function unSyncSel(){
  var one = unSel()[0];
  var bar = document.querySelector('.un-bar-n');
  var go = document.getElementById('unGo');
  var cl = document.getElementById('unClear');
  var od = document.getElementById('unOpenDir');
  if(bar){
    bar.innerHTML = one
      ? '已选 <b>' + unEsc(one.name) + '</b> · ' + unSize(one.bytes || 0)
      : '<span style="color:var(--tx3)">勾一个再卸载 —— 一次只卸一个，卸完自动查残留</span>';
  }
  if(go) go.disabled = !one || UN_BUSY;
  if(cl) cl.disabled = !one;
  /* 「打开目录」一直渲染在底栏里（没选就置灰）—— 别为它整页重绘，重绘会吞点击 */
  if(od) od.disabled = !(one && one.installLoc);
}
/* 残留勾选：只改这一行 + 底栏数字，不重绘 */
function unResToggle(id){
  var r = UN_RES.filter(function(x){ return x.id === id; })[0];
  if(!r) return;
  if(!r.safe){ toast('这条我不自动清：' + (r.guard || '判不准'), 'warn'); return; }
  if(UN_RES_ON[id]) delete UN_RES_ON[id]; else UN_RES_ON[id] = true;
  var el = document.querySelector('[data-unres="' + id + '"]');
  if(el){
    el.classList.toggle('on', !!UN_RES_ON[id]);
    var cb = el.querySelector('.cbx');
    if(cb){ cb.classList.toggle('on', !!UN_RES_ON[id]); cb.innerHTML = UN_RES_ON[id] ? ico('check', 12) : ''; }
  }
  var sel = unResSel();
  var bytes = sel.reduce(function(s, x){ return s + (x.bytes || 0); }, 0);
  var n = document.getElementById('unRN'), go = document.getElementById('unCleanGo');
  if(n) n.textContent = sel.length;
  if(go) go.disabled = !sel.length;
  var all = document.getElementById('unResAll');
  var safe = UN_RES.filter(function(x){ return x.safe; });
  var allOn = safe.length && safe.every(function(x){ return !!UN_RES_ON[x.id]; });
  if(all) all.textContent = allOn ? '全不选' : '全选（确定属于它的）';
  var nb = document.querySelector('.un-bar-n');
  if(nb) nb.innerHTML = '已选 <b id="unRN">' + sel.length + '</b> 项'
    + (bytes ? ' · 可释放 <b style="color:var(--ok)">' + unSize(bytes) + '</b>' : '');
}
/* 2026-10-11：这里原来弹一个「卸载不可逆，再确认一次」的框。用户说是画蛇添足 ——
   控制面板点卸载就是**直接调软件自己的卸载程序**，而那个程序几乎都会自己再问一遍
   「确定要卸载吗 / 是否保留配置」，等于问两遍。去掉弹窗，点了就直接调。
   UWP 内置应用同理（走 Remove-AppxPackage 那条路，系统自己也会提示）。 */
function unRun(){
  var it = unSel()[0];
  if(!it){ toast('先勾选一个要卸载的程序', 'warn'); return; }
  UN_BUSY = true; UN_DONE = false; UN_MARK = {}; UN_RESULT = [];
  UN_CUR = { id: it.id, name: it.name, bytes: it.bytes || 0, uwp: isUwp(it) };
  UN_RES = []; UN_RES_ON = {}; UN_SUM = null; UN_BAK = '';
  unState = 'run'; render();

  var stopAll = false;
  var elNow = document.getElementById('unNow');
  var elTick = document.getElementById('unTick');
  var elSec = document.getElementById('unSec');
  var stopBtn = document.getElementById('unStop');
  if(stopBtn) stopBtn.onclick = function(){
    stopAll = true;
    toast('已结束等待，这就给你看这次的结果', 'info');
  };

  var t0 = Date.now();
  var secT = setInterval(function(){
    if(elSec) elSec.textContent = '已等待 ' + Math.round((Date.now() - t0) / 1000) + ' 秒';
  }, 1000);

  /* kind: wait | run | ok | warn —— 只改图标与文字，不重绘（重绘会吞点击） */
  function setRow(kind, txt){
    var row = document.getElementById('uni0');
    var icel = document.getElementById('upi0');
    var st = document.getElementById('uns0');
    if(row) row.classList.remove('wait');
    if(st) st.textContent = txt;
    if(icel){
      icel.innerHTML = kind === 'run'
        ? '<span class="scan-ring" style="width:16px;height:16px"></span>'
        : ico(kind === 'ok' ? 'check' : (kind === 'warn' ? 'warn' : 'box'), 16);
      icel.style.color = kind === 'ok' ? 'var(--ok)'
        : (kind === 'warn' ? 'var(--warn)' : (kind === 'run' ? 'var(--info)' : 'var(--tx3)'));
    }
  }

  function start(){
    if(elNow) elNow.textContent = UN_CUR.name;
    if(elTick) elTick.textContent = '等待它的卸载程序';
    setRow('run', '已调用它自己的卸载程序…');
    callGo('UninstallApp', UN_CUR.id).then(function(msg){
      if(msg){ done(false, '没能启动：' + msg); return; }
      watch(function(gone, txt){ done(gone, txt); });
    }).catch(function(e){ done(false, '调用失败：' + e); });
  }

  /* 监视 —— 只报观测到的事实：
     · 每 1.5s 问一次后端 PollUninstall，它按进程名实测"卸载程序还在不在跑"
     · 进程一退出才做枚举确认（全量枚举很贵，至少隔 6 秒一次）
     · 确认消失后还要再等 3 秒才扫残留 —— 卸载程序刚退出时系统还在删文件、
       清注册表，立刻扫会把"还没来得及删的"误报成残留 */
  function watch(cb){
    var begin = Date.now(), exited = false, lastCheck = 0, confirmed = 0;
    var t = setInterval(poll, 1500);
    function sec(){ return Math.round((Date.now() - begin) / 1000); }
    function stop(gone, txt){
      clearInterval(t);
      setRow(gone ? 'ok' : 'warn', txt);
      cb(gone, txt);
    }
    function poll(){
      if(stopAll){ stop(false, '已手动结束等待'); return; }
      if(sec() > 480){ stop(false, '等了 8 分钟仍未确认（卸载窗口可能还在等你点）'); return; }
      callGo('PollUninstall').then(function(s){
        if(stopAll || !s) return;
        if((s.procN || 0) > 0){
          exited = false;
          setRow('run', '卸载程序正在运行…（已等待 ' + sec() + ' 秒）');
          return;
        }
        if(!exited){
          exited = true;
          setRow('run', '卸载程序已退出，正在确认…');
          if(elTick) elTick.textContent = '等系统收尾';
        }
        var now = Date.now();
        if(now - lastCheck < 6000) return;
        lastCheck = now;
        Promise.all([callGo('GetWin32Apps'), callGo('GetUwpApps')]).then(function(r){
          if(stopAll) return;
          var all = (r[0] || []).concat(r[1] || []);
          if(!all.some(function(a){ return a.id === UN_CUR.id; })){
            if(!confirmed){          // 第一次确认消失 → 进 3 秒冷静期
              confirmed = Date.now();
              setRow('run', '已确认移除，等系统收尾…');
              return;
            }
            if(Date.now() - confirmed < 3000) return;
            stop(true, '已确认卸载（重新枚举验证）');
          } else if(sec() > 480){
            stop(false, '未确认（卸载向导可能还在等你点）');
          } else {
            setRow('run', '卸载程序已退出，但列表里还在…（已等待 ' + sec() + ' 秒）');
          }
        }).catch(function(){});
      }).catch(function(){});
    }
  }

  /* 收尾：没确认消失就不再往下走（避免把还在用的程序注册表当残留删了）；
     内置应用不扫注册表残留（用户定），确认完直接结束。 */
  function done(gone, txt){
    clearInterval(secT);
    if(gone){
      setRow('ok', '已确认卸载');
      if(elTick) elTick.textContent = '完成';
    } else {
      setRow('warn', txt || '未确认');
    }
    if(!gone || UN_CUR.uwp){ finish(gone); return; }
    setTimeout(scanResidue, 260);
  }

  start();
}

/* 自动扫残留：没残留就直接完成，有就摆清单给你定 */
function scanResidue(){
  unState = 'scan'; render();
  callGo('ScanAppResidue').then(function(items){
    UN_RES = items || [];
    UN_RES_ON = {};
    UN_RES.forEach(function(r){
      /* 默认只勾「确定属于它的注册表项」（Geek 口径）。
         文件夹不默认勾 —— 文件删了没法还原，注册表至少还有 .reg 备份。 */
      if(r.safe && r.kind === 'reg') UN_RES_ON[r.id] = true;
    });
    if(!UN_RES.length){ finish(true); return; }
    callGo('ResidueBackupDir').then(function(d){ UN_BAK = d || ''; }).catch(function(){ UN_BAK = ''; })
      .then(function(){ unState = 'residue'; render(); });
  }).catch(function(e){
    toast('残留扫描失败：' + e, 'warn');
    finish(true);
  });
}

/* 清理勾选的残留（后端会逐条再过一遍安全闸门，不 safe 的它也会拒绝） */
function unDoClean(){
  var ids = unResSel().map(function(r){ return r.id; });
  if(!ids.length){ toast('没有勾选任何残留', 'warn'); return; }
  unState = 'clean'; render();
  callGo('CleanAppResidue', ids).then(function(res){
    res = res || [];
    var fails = [], cleaned = 0, freed = 0;
    res.forEach(function(r){
      if(r.err){ fails.push({ path: r.path, err: r.err }); return; }
      cleaned++; freed += (r.bytes || 0);
    });
    finish(true, { total: ids.length, cleaned: cleaned, freed: freed, fails: fails, skipped: false });
    toast(cleaned ? ('清理完成 ' + cleaned + ' 项') : '一项都没清掉，看详情', cleaned ? 'ok' : 'warn');
  }).catch(function(e){
    toast('清理失败：' + e, 'warn');
    finish(true, { total: ids.length, cleaned: 0, freed: 0, fails: [{ path: '', err: String(e) }], skipped: false });
  });
}

/* 完成页：清没清理都从这里进；列表在这里刷新（结果页用快照渲染，不会被冲掉） */
function finish(gone, cl){
  UN_SUM = {
    name: (UN_CUR && UN_CUR.name) || '', bytes: (UN_CUR && UN_CUR.bytes) || 0,
    gone: !!gone, uwp: !!(UN_CUR && UN_CUR.uwp), resN: UN_RES.length,
    cleaned: cl ? cl.cleaned : 0, freed: cl ? cl.freed : 0,
    fails: cl ? cl.fails : [], skipped: !!(cl && cl.skipped)
  };
  UN_BUSY = false; UN_DONE = true; unState = 'done'; render();
  var bk = document.getElementById('unBack');
  if(bk) bk.onclick = function(){
    UN_DONE = false; UN_RESULT = []; UN_MARK = {};
    UN_RES = []; UN_RES_ON = {}; UN_SUM = null; UN_CUR = null;
    unState = 'ready'; render();
    setTimeout(unFillIcons, 0);   // 列表刚重建，把已缓存的图标填回去
  };
  loadApps(true); // 刷新真实列表；结果页用快照渲染，不会被这次刷新冲掉
}
/* 体积实测：串行（并发会让磁盘 I/O 抖动得厉害），可中止 */
async function unCalcAll(){
  var S = window.UN_SIZE;
  if(S.running) return;
  var list = APPS.filter(function(a){ return a.sizePending && a.installLoc; });
  if(!list.length){ toast('没有需要实测的应用', 'info'); return; }
  S.running = true; S.done = 0; S.total = list.length; S.stop = false;
  render();
  for(var i = 0; i < list.length; i++){
    if(S.stop) break;
    var a = list[i];
    try{
      var r = await callGo('CalcAppSize', a.id);
      if(r){ a.bytes = r.bytes || 0; a.files = r.files || 0; a.sizePending = false; if(r.readable) a.sizeSrc = 'dir'; }
    }catch(e){}
    S.done = i + 1;
    render();
    await new Promise(function(r){ setTimeout(r, 0); });
  }
  var n = S.done, stop = S.stop;
  S.running = false; S.done = 0; S.total = 0; S.stop = false;
  render();
  toast(stop ? ('已中止，实测了 ' + n + ' 个') : ('实测完成 ' + n + ' 个，标注已从「申报/未知」变成「实测」'), 'ok');
}

/* ── 事件委托：挂在 #content 上，重绘不会失效 ───────────────────────────── */
window.bindUninst = function(){
  var root = document.getElementById('content');
  if(!root || root.dataset.unbound) return;
  root.dataset.unbound = '1';

  root.addEventListener('click', function(e){
    var t = e.target;

    var tab = t.closest('[data-untab]');
    if(tab){ unTab = tab.getAttribute('data-untab'); render(); unFillIcons(); return; }

    var srt = t.closest('[data-unsort]');
    if(srt){
      var k = srt.getAttribute('data-unsort');
      if(unSort === k) unAsc = !unAsc;
      else { unSort = k; unAsc = (k === 'name' || k === 'publisher' || k === 'version'); }
      render(); unFillIcons(); return;
    }

    /* 没有「表头全选」了：一次只卸一个（2026-10-11 用户定） */

    var pick = t.closest('[data-unpick]');
    if(pick){
      var idx = +pick.getAttribute('data-unpick');
      var want = !APPS[idx].on;
      unPick(idx, want);   // 单选：内部已把其它项的勾选态清掉
      /* 视觉也要跟着清 —— 只改 class，不整页重绘（重绘会吞点击，项目铁律） */
      [].forEach.call(document.querySelectorAll('.un-row'), function(el){
        el.classList.remove('sel');
        var c = el.querySelector('.cbx');
        if(c){ c.classList.remove('on'); c.innerHTML = ''; }
      });
      if(want){
        pick.classList.add('on');
        pick.innerHTML = ico('check', 12);
        var row = pick.closest('[data-unrow]');
        if(row) row.classList.add('sel');
      }
      return;
    }

    var one = t.closest('[data-unone]');
    if(one){
      var i2 = +one.getAttribute('data-unone');
      APPS.forEach(function(a){ a.on = false; });
      APPS[i2].on = true;
      unRun();
      return;
    }

    /* 残留清单：勾一条 / 全选 / 清理 / 跳过 */
    var res = t.closest('[data-unres]');
    if(res){ unResToggle(res.getAttribute('data-unres')); return; }

    if(t.closest('#unResAll')){
      var safe = UN_RES.filter(function(x){ return x.safe; });
      var allOn = safe.length && safe.every(function(x){ return !!UN_RES_ON[x.id]; });
      safe.forEach(function(x){ if(allOn) delete UN_RES_ON[x.id]; else UN_RES_ON[x.id] = true; });
      render(); return;   // 只在这一处整页重绘：按钮点击已完成，不存在吞点击的问题
    }
    if(t.closest('#unCleanGo')){ unDoClean(); return; }
    if(t.closest('#unCleanSkip')){
      finish(true, { total: UN_RES.length, cleaned: 0, freed: 0, fails: [], skipped: true });
      toast('没有清理任何残留', 'info');
      return;
    }

    if(t.closest('#unGo')){ unRun(); return; }
    if(t.closest('#unClear')){ APPS.forEach(function(a){ a.on = false; }); render(); unFillIcons(); return; }
    if(t.closest('#unSizeAll')){ unCalcAll(); return; }
    if(t.closest('#unRetry') || t.closest('#unBack')){
      unState = 'loading'; APPS.forEach(function(a){ a.on = false; }); UN_MARK = {}; render(); loadApps(); return;
    }
    if(t.closest('#unOpenDir')){
      var s0 = unSel()[0]; if(!s0) return;
      callGo('OpenAppFolder', s0.id).then(function(msg){ if(msg) toast(msg, 'warn'); }).catch(function(){});
      return;
    }
  });

  /* 搜索：输入只重画表格区（整页重绘会把焦点弄丢，字打到一半就没了） */
  root.addEventListener('input', function(e){
    if(e.target && e.target.id === 'unQ'){
      unQuery = e.target.value;
      var box = document.querySelector('.un-tbl');
      if(box){
        var list = unSorted();
        box.innerHTML = unHeadHtml()
          + (list.length ? list.map(unRowHtml).join('')
              : '<div class="empty" style="padding:34px 20px"><div class="empty-t">没有匹配的软件</div>'
                + '<div class="empty-d">换个词试试，或清空搜索框。</div></div>');
      }
      unFillIcons();
    }
  });
};

/* 页面进入 / 重绘后补图标：render() 在主文件里，这里靠调用方在合适时机触发。
   简单可靠的做法：每次 render 之后异步填一次（用微任务，不阻塞渲染）。 */
if(typeof window !== 'undefined'){
  var __unOrigRender = window.render;
  window.render = function(){
    var r = __unOrigRender.apply(this, arguments);
    try{
      if(cur === 'appx' && unState === 'ready') setTimeout(unFillIcons, 0);
    }catch(e){}
    return r;
  };
}
