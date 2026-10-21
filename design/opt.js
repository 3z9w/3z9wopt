/* ============================================================================
   opt.js —— 系统优化页（2026-10-11 重新设计 + 从主文件拆成独立模块）
   ---------------------------------------------------------------------------
   载入方式与 pcinfo.js 完全一致（见主文件 <head> 里的「系统优化页独立文件加载器」）：
   主文件用**同步 XHR** 读本文件，再**直接 eval** 进主文件作用域 ——
   这样 window.renderOpt / window.loadOptCatalog 才在 PAGES、reloadCurrentPage
   引用它们之前就位（外链 <script src> 是异步的，会晚于主脚本，等于没定义）。

   界面三件事与后端一一对应：
     · 勾选后「应用」    → ApplyOptimize(ids)        （批量应用）
     · 点已优化项        → RestoreOptimize([id])     （单项还原）
     · 「全部恢复」      → RestoreOptimize(全部已优化项)
     · 「基本/深度优化」 → 先清空选择，再一键勾上对应预设的项（不直接执行）
   应用过程由后端事件 opt:progress 回报，进度条在主文件 bindGoEvents 里更新。

   ⚠️ 勾选、展开、右栏移除**不做整页 render()**：
      整页重建会换掉节点，而浏览器只在 mousedown/mouseup 命中同一元素时才派发 click ——
      数据回来那一刻的整页重绘会「吞点击」，点了没反应还不报错（垃圾清理模块踩过这个坑）。
      这里只改那一个节点 + 右栏 + 计数，见 optToggle / optSyncSide。
      只有「筛选、预设、全选、重置、应用完成」这些一次性动作才整页重绘。

   ⚠️ 行内只放**已经存在的**框架类（chip / chip ok / chip warn / chip info / btn / card /
      bar / empty / page-head…），样式全在 design/opt.css，颜色全走全局主题变量。
   ============================================================================ */

/* ⚠️ OPT / OPT_SEL / OPT_BUSY / OPT_OPEN 必须挂在 window 上：
   主文件的事件监听器（opt:progress）要写的是同一个 OPT_BUSY。加载器是
   (function(){ … eval(code) … })()，本文件在里面的 IIFE 作用域中执行 ——
   用 var 声明的话主文件那份赋值会落到另一个同名全局变量上，进度条永远停在 0。
   其余只在模块内部用的状态照旧用 var。 */
window.OPT = window.OPT || [];
window.OPT_SEL = window.OPT_SEL || {};
window.OPT_BUSY = window.OPT_BUSY || null;
window.OPT_OPEN = window.OPT_OPEN || {};
var optState = 'loading', optErr = '', optQuery = '';

/* ── 小工具 ─────────────────────────────────────────────────────────────── */

function optEsc(s){
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
function optById(id){
  for(var i = 0; i < OPT.length; i++){ if(OPT[i].id === id) return OPT[i]; }
  return null;
}
function optCounts(){
  return {
    total: OPT.length,
    on: OPT.filter(function(i){ return i.optimized; }).length,
    basic: OPT.filter(function(i){ return i.basic; }).length,
    deep: OPT.filter(function(i){ return i.deep; }).length,
    reboot: OPT.filter(function(i){ return i.reboot; }).length
  };
}
function optHit(it){
  /* 2026-10-11：原来这里还有 basic/deep 的预设筛选（「全部/基本/深度」三个切换按钮）。
     已在用户要求下删除 —— 底部操作条本来就有「基本优化 / 深度优化」两个按钮，两套入口重复。
     筛选只留搜索框，别再把 optFilter 加回来。 */
  if(optQuery){
    var q = optQuery.toLowerCase();
    var hay = ((it.name || '') + ' ' + (it.desc || '') + ' ' + (it.id || '')).toLowerCase();
    if(hay.indexOf(q) < 0) return false;
  }
  return true;
}
function optGroups(){
  var order = [], m = {};
  OPT.forEach(function(it){
    if(!optHit(it)) return;
    var k = it.catName || it.cat || '其它';
    if(!m[k]){ m[k] = []; order.push(k); }
    m[k].push(it);
  });
  return order.map(function(k){ return { g: k, items: m[k] }; });
}
function optSelIds(){ return Object.keys(OPT_SEL).filter(function(k){ return OPT_SEL[k]; }); }
function optRowEl(id){
  var all = document.querySelectorAll('.opt-row');
  for(var i = 0; i < all.length; i++){ if(all[i].getAttribute('data-opt') === id) return all[i]; }
  return null;
}
function optCatEl(name){
  var all = document.querySelectorAll('.opt-cat');
  for(var i = 0; i < all.length; i++){ if(all[i].getAttribute('data-cat') === name) return all[i]; }
  return null;
}

/* ── 数据 ───────────────────────────────────────────────────────────────── */

window.loadOptCatalog = function(){
  if(!hasGo()){ if(inWails()){ optState = 'failed'; optErr = 'window.go 没拿到（后端未连接）'; } return; }
  optState = 'loading';
  callGo('GetOptimizeCatalog').then(function(list){
    OPT = list || []; optState = 'ready';
    if(cur === 'optimize') render();
  }).catch(function(e){
    optState = 'failed'; optErr = String(e);
    if(cur === 'optimize') render();
  });
};

/* ── 渲染：小块 ─────────────────────────────────────────────────────────── */

/* 勾与状态徽标（三态互斥：已优化 > 待应用 > 未动） */
function optChipsHtml(it){
  var done = !!it.optimized, sel = !!OPT_SEL[it.id] && !done, ch = [];
  if(done) ch.push('<span class="chip ok">已优化</span>');
  else if(sel) ch.push('<span class="chip info">待应用</span>');
  if(it.reboot) ch.push('<span class="chip warn">需重启</span>');
  if(it.deep && !it.basic) ch.push('<span class="chip">深度</span>');
  return ch.join('');
}
function optRowHtml(it, ix){
  var done = !!it.optimized, sel = !!OPT_SEL[it.id] && !done;
  var tip = done ? '点一下还原这一项' : (sel ? '点一下取消勾选' : '点一下勾上，之后再点「应用」');
  return '<div class="opt-row' + (done ? ' done' : (sel ? ' sel' : '')) + '" data-opt="' + optEsc(it.id) + '"'
    + ' role="switch" aria-checked="' + (done || sel ? 'true' : 'false') + '" tabindex="0"'
    + ' style="animation-delay:' + Math.min((ix || 0) * 14, 180) + 'ms" title="' + optEsc(tip) + '">'
    + '<span class="opt-box"><svg width="11" height="11" viewBox="0 0 12 12" fill="none" aria-hidden="true">'
    +   '<path d="M2 6.4 4.7 9 10 3.2" stroke="#fff" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"/></svg></span>'
    + '<span class="opt-row-t"><span class="opt-row-n">' + optEsc(it.name) + '</span>'
    +   (it.desc ? '<span class="opt-row-d">' + optEsc(it.desc) + '</span>' : '')
    + '</span>'
    + '<span class="opt-chips">' + optChipsHtml(it) + '</span>'
  + '</div>';
}
function optCatHtml(g){
  var on = g.items.filter(function(i){ return i.optimized; }).length;
  var gp = g.items.length ? Math.round(on * 100 / g.items.length) : 0;
  /* 搜索时一律展开，否则搜出来的项藏在折叠卡里，等于没搜 */
  var open = !!optQuery || !!OPT_OPEN[g.g];
  return '<div class="opt-cat' + (open ? ' open' : '') + '" data-cat="' + optEsc(g.g) + '">'
    + '<div class="opt-cat-h" data-optcat="' + optEsc(g.g) + '">'
    +   '<span class="opt-caret"></span>'
    +   '<span class="opt-cat-n">' + optEsc(g.g) + '</span>'
    +   '<span class="opt-cat-s">已优化 <b>' + on + '</b> / ' + g.items.length + '</span>'
    +   '<span class="opt-prog"><i style="width:' + gp + '%"></i></span>'
    + '</div>'
    + '<div class="opt-cat-b">' + g.items.map(optRowHtml).join('') + '</div>'
  + '</div>';
}
function optListHtml(){
  var groups = optGroups();
  if(!groups.length){
    return '<div class="card"><div class="empty" style="padding:34px 20px">'
      + '<div class="empty-t">没有匹配的优化项</div>'
      + '<div class="empty-d">换个词试试，或者把筛选切回「全部」。</div></div></div>';
  }
  return groups.map(optCatHtml).join('');
}
function optSideListHtml(){
  var ids = optSelIds();
  if(!ids.length){
    /* 第二行是**操作指引**，用 --info 蓝高亮：跟「已勾选」的蓝是同一个语义色，
       提醒用户"勾了还没执行"，别让它跟上面那行灰字一样沉下去看不见。 */
    return '<div class="opt-side-e">还没勾选任何项<br>'
      + '<span class="opt-side-tip">左边点一下就是勾上</span></div>';
  }
  return ids.map(function(id){
    var it = optById(id);
    if(!it) return '';
    return '<div class="opt-side-i" style="animation-delay:' + Math.min(ids.indexOf(id) * 12, 160) + 'ms">'
      + '<span title="' + optEsc(it.name) + '">' + optEsc(it.name) + '</span>'
      + '<span class="opt-side-x" data-optunsel="' + optEsc(id) + '" title="移出待应用列表">&times;</span>'
    + '</div>';
  }).join('');
}

/* ── 渲染：整页 ─────────────────────────────────────────────────────────── */

window.renderOpt = function(){
  if(optState === 'failed'){
    return '<div class="page-head"><div class="page-title">系统优化</div>'
      + '<div class="page-sub">优化项清单未获取到。</div></div>'
      + '<div class="card"><div class="empty" style="padding:48px 20px">'
      + '<div class="empty-ico" style="color:var(--danger)">' + ico('warn', 28) + '</div>'
      + '<div class="empty-t">没能读到优化项清单</div><div class="empty-d">' + optEsc(optErr) + '</div>'
      + '<div style="display:flex;justify-content:center;margin-top:14px">'
      +   '<button class="btn primary" id="optRetry">' + ico('refresh', 16) + '重新读取</button></div>'
      + '</div></div>';
  }
  if(optState !== 'ready'){
    return '<div class="page-head"><div class="page-title">系统优化</div>'
      + '<div class="page-sub">正在读取优化项清单…</div></div>'
      + '<div class="card"><div class="empty" style="padding:56px 20px">'
      + '<div class="empty-ico" style="color:var(--info)">' + ico('refresh', 28) + '</div>'
      + '<div class="empty-t">正在读优化项…</div></div></div>';
  }

  var c = optCounts(), sel = optSelIds();
  var selReboot = OPT.filter(function(i){ return OPT_SEL[i.id] && i.reboot; }).length;
  var R = 26, CIRC = 2 * Math.PI * R;
  var frac = c.total ? c.on / c.total : 0;
  var offv = (CIRC * (1 - frac)).toFixed(1);
  var pct = Math.round(frac * 100);

  var head = '<div class="page-head" style="margin-bottom:12px">'
    + '<div class="page-title">系统优化</div>'
    + '<div class="page-sub">共 <b>' + c.total + '</b> 项可改，按分类折叠在下面。勾选只是排队，'
    + '点右下角「应用」才生效；已经生效的项，点它左边的勾就还原。</div></div>';

  var hero = '<div class="opt-hero">'
    + '<div class="opt-hero-fig">' + ico('sliders', 23) + '</div>'
    + '<div class="opt-kpis">'
    +   '<div class="opt-kpi"><b>' + c.total + '</b><span>优化项总数</span></div>'
    +   '<div class="opt-kpi"><b style="color:var(--ok)">' + c.on + '</b><span>已经优化</span></div>'
    +   '<div class="opt-kpi"><b style="color:var(--info)" id="optSelN">' + sel.length + '</b><span>已勾选待应用</span></div>'
    + '</div>'
    + '<div class="opt-hero-r">'
    +   '<span class="chip">基本预设 ' + c.basic + '</span>'
    +   '<span class="chip">深度预设 ' + c.deep + '</span>'
    +   (c.reboot ? '<span class="chip warn">需重启 ' + c.reboot + ' 项</span>' : '')
    + '</div>'
  + '</div>';

  /* 搜索框**独占一整行**：宽度与上面 hero 卡、下面操作条完全一致（用户 2026-10-11 明确要求）。
     筛选按钮不能跟它同一行 —— 同排会把输入框挤成 2/3 宽，看起来就跟 hero 不齐了（用户实拍指出过）。
     所以拆两行：先搜索，再一排筛选项。 */
  var tools = '<div class="opt-tools">'
    + '<label class="opt-search">'
    +   '<svg width="14" height="14" viewBox="0 0 14 14" fill="none" aria-hidden="true">'
    +     '<circle cx="6" cy="6" r="4.2" stroke="currentColor" stroke-width="1.5"/>'
    +     '<path d="M9.3 9.3 12.3 12.3" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>'
    +   '<input id="optSearch" type="text" autocomplete="off" spellcheck="false"'
    +     ' placeholder="搜索：任务栏 / 休眠 / 遥测 / 服务…" value="' + optEsc(optQuery) + '">'
    + '</label>'
  + '</div>';

  var side = '<aside class="opt-side">'
    + '<div class="opt-side-h">'
    +   '<div class="opt-ring">'
    +     '<svg width="62" height="62" viewBox="0 0 62 62" aria-hidden="true">'
    +       '<defs><linearGradient id="optRingG" x1="0" y1="0" x2="1" y2="1">'
    +         '<stop offset="0" stop-color="#3d6bf5"/><stop offset=".55" stop-color="#22d3ee"/>'
    +         '<stop offset="1" stop-color="#a78bfa"/></linearGradient></defs>'
    +       '<circle cx="31" cy="31" r="' + R + '" fill="none" stroke="var(--stroke)" stroke-width="5"/>'
    +       '<circle class="rg" cx="31" cy="31" r="' + R + '" fill="none" stroke="url(#optRingG)" stroke-width="5"'
    +         ' stroke-linecap="round" stroke-dasharray="' + CIRC.toFixed(1) + '" stroke-dashoffset="' + offv + '"'
    +         ' transform="rotate(-90 31 31)"/>'
    +     '</svg>'
    +     '<span class="opt-ring-tx">' + pct + '%</span>'
    +   '</div>'
    +   '<div style="min-width:0">'
    +     '<div class="opt-side-t">本机已优化</div>'
    +     '<div class="opt-side-b"><b>' + c.on + '</b> / ' + c.total + ' 项</div>'
    +   '</div>'
    + '</div>'
    + '<div class="opt-side-l" id="optSideL">' + optSideListHtml() + '</div>'
    + '<div class="opt-side-f">已经优化的项，点它左边的勾就退回原状。'
    +   '<span id="optSideNote">' + (selReboot ? '<br><b>其中 ' + selReboot + ' 项要重启电脑才完全生效。</b>' : '') + '</span>'
    + '</div>'
  + '</aside>';

  var run = OPT_BUSY ? ('<div class="opt-run">'
    + '<div class="opt-run-h"><b id="optNow">' + optEsc(OPT_BUSY.name) + '</b>'
    + '<em id="optPct">' + OPT_BUSY.done + ' / ' + OPT_BUSY.total + '</em></div>'
    + '<div class="bar"><i id="optBar" style="width:'
    +   (OPT_BUSY.total ? Math.round(OPT_BUSY.done / OPT_BUSY.total * 100) : 0) + '%"></i></div>'
    + '</div>') : '';

  var bar = '<div class="opt-bar">'
    /* 文案用「一键勾选」不用「一键执行」：点它只是把该预设的项勾上、放进右栏待确认，
       不直接改系统（要改还得点右边「应用 N 项」）。2026-10-11 用户定的措辞，别改成"执行"。 */
    + '<button class="btn sm opt-pre" data-optpre="basic">' + ico('bolt', 14) + '一键勾选基本优化 ' + c.basic + '</button>'
    + '<button class="btn sm opt-pre" data-optpre="deep">' + ico('bolt', 14) + '一键勾选深度优化 ' + c.deep + '</button>'
    + '<button class="btn sm" data-optall>' + ico('grid', 14) + '全选</button>'
    + '<button class="btn sm" data-optreset>重置</button>'
    + '<span class="opt-bar-sep"></span>'
    + '<span class="opt-bar-n">已勾选 <b id="optSelN2">' + sel.length + '</b> 项</span>'
    + '<span class="opt-bar-sp"></span>'
    + (c.on ? '<button class="btn sm" id="optRestoreAll">' + ico('refresh', 14) + '全部恢复 ' + c.on + ' 项</button>' : '')
    + '<button class="btn sm primary" id="optApply"' + (sel.length && !OPT_BUSY ? '' : ' disabled') + '>'
    +   ico('check', 14) + '应用 ' + sel.length + ' 项</button>'
  + '</div>';

  /* 工具条在 .opt-wrap **外面**：这样它跟上面概览卡、下面操作条一样整排宽（上下左右都对齐）。
     ⚠️ 也不能挪进 .opt-list —— 搜索时 optRepaintList() 换的是 .opt-list 的 innerHTML，
     进去就会被重建、输入框丢焦点，字打到一半就没了。 */
  return head + hero + tools
    + '<div class="opt-wrap"><div class="opt-list">' + optListHtml() + '</div>' + side + '</div>'
    + '<div class="opt-sp"></div>' + run + bar;
};

/* ── 局部刷新（不整页重绘） ─────────────────────────────────────────────── */

/* 搜索时只重画清单区：整页重建会让输入框失去焦点，打到一半的字就没了 */
function optRepaintList(){
  var box = document.querySelector('.opt-list');
  if(box) box.innerHTML = optListHtml();
}
/* 右栏、三个计数、底部按钮一起对齐当前选择 */
function optSyncSide(){
  var sel = optSelIds();
  var selReboot = OPT.filter(function(i){ return OPT_SEL[i.id] && i.reboot; }).length;
  var L = document.getElementById('optSideL');
  if(L) L.innerHTML = optSideListHtml();
  ['optSelN', 'optSelN2'].forEach(function(id){
    var e = document.getElementById(id); if(e) e.textContent = sel.length;
  });
  var note = document.getElementById('optSideNote');
  if(note) note.innerHTML = selReboot ? ('<br><b>其中 ' + selReboot + ' 项要重启电脑才完全生效。</b>') : '';
  var ap = document.getElementById('optApply');
  if(ap){
    ap.disabled = !(sel.length && !OPT_BUSY);
    ap.innerHTML = ico('check', 14) + '应用 ' + sel.length + ' 项';
  }
}
function optToggle(id, on){
  var it = optById(id);
  if(!it || it.optimized) return;
  if(on) OPT_SEL[id] = true; else delete OPT_SEL[id];
  var row = optRowEl(id);
  if(row){
    row.classList.toggle('sel', !!on);
    row.setAttribute('aria-checked', on ? 'true' : 'false');
    row.setAttribute('title', on ? '点一下取消勾选' : '点一下勾上，之后再点「应用」');
    var cs = row.querySelector('.opt-chips');
    if(cs) cs.innerHTML = optChipsHtml(it);
  }
  optSyncSide();
}
function optFlipCat(name){
  OPT_OPEN[name] = !OPT_OPEN[name];
  var el = optCatEl(name);
  if(el) el.classList.toggle('open', !!OPT_OPEN[name]);
}

/* ── 一次性动作（这些可以整页重绘） ─────────────────────────────────────── */

/* 「基本优化 / 深度优化」= 先清空选择，再一键勾上该预设的项（**不直接执行**，还能反悔）。
   已经生效的项不进待应用队列 —— 它们本来就是优化态，勾上没有意义。 */
function optPreset(which){
  OPT_SEL = {};
  var n = 0;
  OPT.forEach(function(it){
    var inPre = (which === 'basic') ? it.basic : it.deep;
    if(inPre && !it.optimized){ OPT_SEL[it.id] = true; n++; }
  });
  render();
  toast(n ? ('已按「' + (which === 'basic' ? '基本' : '深度') + '预设」勾选 ' + n + ' 项，点右下角「应用」生效')
          : '该预设里的项都已经优化过了，没有要应用的', n ? 'info' : 'ok');
}
/* 「全选」= 勾上当前筛选/搜索出来的全部项（不含已优化的） */
function optSelectAll(){
  var n = 0;
  optGroups().forEach(function(g){
    g.items.forEach(function(it){ if(!it.optimized){ OPT_SEL[it.id] = true; n++; } });
  });
  if(!n){ toast('当前列表里没有可勾选的项', 'info'); return; }
  render();
  toast('已勾选 ' + n + ' 项', 'info');
}
function optReset(){
  if(!optSelIds().length){ toast('还没有勾选任何项', 'info'); return; }
  OPT_SEL = {};
  render();
}

/* ── 应用 / 还原 ───────────────────────────────────────────────────────── */

function optAskApply(){
  var ids = optSelIds();
  if(!ids.length){ toast('先勾选要应用的项', 'warn'); return; }
  var reboot = OPT.filter(function(i){ return OPT_SEL[i.id] && i.reboot; }).length;
  var box = document.getElementById('modalBox');
  var doIt = function(){
    OPT_BUSY = { done: 0, total: ids.length, name: '准备中…' };
    if(cur === 'optimize') render();
    callGo('ApplyOptimize', ids).then(function(res){
      var ok = (res || []).filter(function(r){ return r.ok; }).length;
      var bad = (res || []).filter(function(r){ return !r.ok; });
      OPT_BUSY = null; OPT_SEL = {};
      toast('已应用 ' + ok + ' / ' + ids.length + ' 项' + (bad.length ? ('，失败 ' + bad.length + ' 项') : ''),
            bad.length ? 'warn' : 'ok');
      loadOptCatalog();
    }).catch(function(e){
      OPT_BUSY = null;
      toast('应用失败：' + e, 'danger');
      if(cur === 'optimize') render();
    });
  };
  if(!box){ doIt(); return; }
  box.innerHTML = '<div class="modal-t">应用这 ' + ids.length + ' 项优化？</div>'
    + '<div style="font-size:12.5px;line-height:1.85;color:var(--tx2)">'
    +   '<b>会发生什么</b>：按你勾选的项修改系统设置（服务、计划任务、注册表等）<br>'
    +   '<b>影响面多大</b>：只改这 ' + ids.length + ' 项，其它一律不动'
    +   (reboot ? ('；其中 <b>' + reboot + '</b> 项要重启才完全生效') : '') + '<br>'
    +   '<b>怎么撤回</b>：每一项都能单独还原 —— 已经优化过的项，点它的勾就退回去了'
    + '</div>'
    + '<div class="modal-f"><button class="btn" id="optCancel">再想想</button>'
    + '<button class="btn primary" id="optOk">' + ico('check', 16) + '开始应用</button></div>';
  document.getElementById('modal').classList.add('open');
  document.getElementById('optCancel').onclick = closeModal;
  document.getElementById('optOk').onclick = function(){ closeModal(); doIt(); };
}
function optAskRestoreOne(id){
  var it = optById(id);
  if(!it) return;
  var box = document.getElementById('modalBox');
  var doIt = function(){
    callGo('RestoreOptimize', [id]).then(function(res){
      var r = (res || [])[0] || {};
      toast(r.ok ? ('已还原：' + it.name) : ('还原失败：' + (r.err || '未知原因')), r.ok ? 'ok' : 'danger');
      loadOptCatalog();
    }).catch(function(e){ toast('还原失败：' + e, 'danger'); });
  };
  if(!box){ doIt(); return; }
  box.innerHTML = '<div class="modal-t">还原这一项？</div>'
    + '<div class="modal-d">' + optEsc(it.name) + '</div>'
    + '<div style="font-size:12.5px;line-height:1.85;color:var(--tx2)">'
    +   '<b>会发生什么</b>：把它改回系统原来的状态<br>'
    +   '<b>影响面多大</b>：只这一项，其它优化保持现状<br>'
    +   '<b>怎么撤回</b>：还原本身也会记进操作日志'
    + '</div>'
    + '<div class="modal-f"><button class="btn" id="optCancel">取消</button>'
    + '<button class="btn primary" id="optOk">' + ico('check', 16) + '还原</button></div>';
  document.getElementById('modal').classList.add('open');
  document.getElementById('optCancel').onclick = closeModal;
  document.getElementById('optOk').onclick = function(){ closeModal(); doIt(); };
}
function optRestoreAll(){
  var ids = OPT.filter(function(i){ return i.optimized; }).map(function(i){ return i.id; });
  if(!ids.length){ toast('没有已优化的项', 'info'); return; }
  var box = document.getElementById('modalBox');
  var doIt = function(){
    OPT_BUSY = { done: 0, total: ids.length, name: '正在全部恢复…' };
    if(cur === 'optimize') render();
    callGo('RestoreOptimize', ids).then(function(res){
      var ok = (res || []).filter(function(r){ return r.ok; }).length;
      OPT_BUSY = null;
      toast('已恢复 ' + ok + ' / ' + ids.length + ' 项', ok === ids.length ? 'ok' : 'warn');
      loadOptCatalog();
    }).catch(function(e){
      OPT_BUSY = null;
      toast('恢复失败：' + e, 'danger');
      if(cur === 'optimize') render();
    });
  };
  if(!box){ doIt(); return; }
  box.innerHTML = '<div class="modal-t">把 ' + ids.length + ' 项优化全部恢复？</div>'
    + '<div style="font-size:12.5px;line-height:1.85;color:var(--tx2)">'
    +   '<b>会发生什么</b>：把这 ' + ids.length + ' 项逐个改回系统原状<br>'
    +   '<b>影响面多大</b>：只动这些已优化的项<br>'
    +   '<b>怎么撤回</b>：恢复后想再优化，重新勾选应用即可'
    + '</div>'
    + '<div class="modal-f"><button class="btn" id="optCancel">取消</button>'
    + '<button class="btn primary" id="optOk">' + ico('check', 16) + '全部恢复</button></div>';
  document.getElementById('modal').classList.add('open');
  document.getElementById('optCancel').onclick = closeModal;
  document.getElementById('optOk').onclick = function(){ closeModal(); doIt(); };
}

/* ── 事件绑定 ───────────────────────────────────────────────────────────── */
/* 一律**事件委托挂在 #content**（只换它的 innerHTML，这个容器本身永不重建）：
   整页 render() 之后按钮还是活的，不需要每次重新绑定 —— 这是本项目定下的做法。 */
window.bindOpt = function(){
  var root = document.getElementById('content');
  if(!root || root.dataset.optBound === '1') return;
  root.dataset.optBound = '1';

  root.addEventListener('click', function(e){
    var t = e.target;
    if(!t || !t.closest) return;

    var ch = t.closest('[data-optcat]');
    if(ch){ optFlipCat(ch.getAttribute('data-optcat')); return; }

    var un = t.closest('[data-optunsel]');
    if(un){ optToggle(un.getAttribute('data-optunsel'), false); return; }

    var pre = t.closest('[data-optpre]');
    if(pre){ optPreset(pre.getAttribute('data-optpre')); return; }
    if(t.closest('[data-optall]')){ optSelectAll(); return; }
    if(t.closest('[data-optreset]')){ optReset(); return; }

    var row = t.closest('.opt-row');
    if(row){
      var id = row.getAttribute('data-opt'), it = optById(id);
      if(!it) return;
      if(it.optimized){ optAskRestoreOne(id); return; }   /* 已优化的：点一下 = 还原这一项 */
      optToggle(id, !OPT_SEL[id]);
      return;
    }

    if(t.closest('#optApply')){ optAskApply(); return; }
    if(t.closest('#optRestoreAll')){ optRestoreAll(); return; }
    if(t.closest('#optRetry')){ optState = 'loading'; render(); loadOptCatalog(); }
  });

  /* 搜索：输入即筛，但只重画清单区（整页重绘会把焦点弄丢，字打到一半就没了） */
  root.addEventListener('input', function(e){
    if(e.target && e.target.id === 'optSearch'){
      optQuery = e.target.value;
      optRepaintList();
    }
  });
};
