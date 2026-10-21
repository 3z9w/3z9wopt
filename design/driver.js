/* ══════════════════════════════════════════════════════════════════════════
   驱动管理 · 独立模块（design/driver.js）
   ---------------------------------------------------------------------------
   功能口径 100% 照老版本（v2 驱动管理）：选系统 → 按设备类别看驱动 → 勾选导出、
   从文件夹添加驱动；**不做删除**（删错关键驱动会开不了机，真要删去设备管理器，
   提示条里那个链接点了直接拉起 devmgmt.msc，不是一句空话）。
   后端接口本版 drivers.go 与老版本完全同名（GetOSSystems / GetDrivers /
   ExportDrivers / ExportAllDrivers / AddDriver / SelectFolder），前端按 1.1 重写。
   视觉按 1.1：玻璃卡 + 蓝为主（配色基准 = 本机配置页），琥珀只留给风险提示条。
   ⚠️ 状态与函数一律挂 window（加载器是 IIFE 包着的 eval，见 ui-prototype.html 的
      「驱动管理页独立文件加载器」），否则 PAGES 引用不到 → 整页白屏。
   ══════════════════════════════════════════════════════════════════════════ */

var DRV_S = {loaded:false, loading:false, err:'', sys:'online', systems:[], showAll:false, all:[], busy:false};

function drvEsc(s){ return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
  return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];
});}
function drvEl(id){ return document.getElementById(id); }
function drvEach(sel, fn){
  var l = document.querySelectorAll(sel);
  for(var i = 0; i < l.length; i++) fn(l[i], i);
}
/* 当前选中的系统对象（枚举不到系统时也要有个能用的说法，不能 undefined） */
function drvCurSys(){
  for(var i = 0; i < DRV_S.systems.length; i++){
    if(DRV_S.systems[i].id === DRV_S.sys) return DRV_S.systems[i];
  }
  return {id:DRV_S.sys, name:'当前系统', path:'', online:DRV_S.sys.indexOf('offline:') !== 0};
}
function drvIsOffline(){ return DRV_S.sys.indexOf('offline:') === 0; }

/* ── 页面骨架 ───────────────────────────────────────────────────────────── */
function renderDriver(){
  return '<div class="page-head"><div class="page-title">驱动管理</div>'
    + '<div class="page-sub">把这台机器上当前系统（也可以选择其他系统）的驱动<b>导出来留个底</b>，'
    + '重装系统后照着列表装回去——这可以<b>快速备份和还原驱动</b>！</div></div>'

    /* hero：一眼看到「多少驱动 / 分几类 / 选了几个」，也解释数据从哪来 */
    + '<div class="drv-hero">'
    +   '<div class="drv-hero-ico">' + ico('cpu', 21) + '</div>'
    +   '<div class="drv-hero-main">'
    +     '<div class="drv-hero-t">所选系统的驱动包，按设备类别归组</div>'
    +     '<div class="drv-hero-d">第三方驱动由系统自带的 pnputil / 驱动存储枚举；勾上右上角<b>「显示内置驱动」</b>，'
    +     '会连系统自带、且这台机<b>正在用</b>的驱动一起列出。</div>'
    +   '</div>'
    +   '<div class="drv-kpis">'
    +     '<div class="drv-kpi"><span>驱动总数</span><b id="drvKAll">—</b></div>'
    +     '<div class="drv-kpi"><span>设备类别</span><b id="drvKCats">—</b></div>'
    +     '<div class="drv-kpi" id="drvKSelBox"><span>已勾选</span><b id="drvKSel">0</b></div>'
    +   '</div>'
    + '</div>'

    /* 系统条：左边选"看哪台机"，右上角「显示内置驱动」+ 重新枚举 */
    + '<div class="drv-bar">'
    +   '<div class="drv-syswrap">'
    +     '<span class="lb">' + ico('monitor', 14) + '操作系统</span>'
    +     '<select id="drvSys">' + drvSysOptions() + '</select>'
    +     '<span class="drv-syshint" id="drvSysHint"></span>'
    +   '</div>'
    +   '<div class="drv-acts">'
    +     '<label class="drv-chkall" title="列系统自带、且这台机正在用的驱动（驱动存储里没用上的包不列）">'
    +     '<input type="checkbox" id="drvShowAll"' + (DRV_S.showAll ? ' checked' : '') + '>显示内置驱动</label>'
    +     '<button class="drv-btn" data-drv-reload id="drvReload">' + ico('refresh', 14) + '重新枚举</button>'
    +   '</div>'
    + '</div>'

    /* 风险提示：设备管理器做成真链接（对应 Dism++ 顶部那句红字提醒） */
    + '<div class="drv-tip">' + ico('warn', 15)
    +   '<span>本页只做<b>查看 · 导出 · 添加</b>。真要删除驱动请到 <a data-drv-devmgr title="点这里直接打开 Windows 设备管理器">设备管理器</a>，'
    +   '删错关键驱动会导致电脑无法启动。</span></div>'

    /* 驱动树：吃掉剩余高度，点类别名或小三角展开 / 收缩（不再给"展开全部"按钮） */
    + '<div class="drv-tree" id="drvTree">' + drvEmptyHtml() + '</div>'

    /* 导出进度（进行时展开） */
    + '<div class="drv-prog" id="drvProg">'
    +   '<div class="drv-prog-txt" id="drvProgTxt">正在导出…</div>'
    +   '<div class="drv-prog-bar"><i id="drvProgBar"></i></div>'
    + '</div>'

    /* 底部操作栏（对齐 Dism++：左边「全选」，右边 导出驱动 / 添加驱动） */
    + '<div class="drv-foot">'
    +   '<button class="drv-btn" data-drv-selall id="drvSelAll">全选</button>'
    +   '<span class="sp"></span>'
    +   '<button class="drv-btn pri" data-drv-export id="drvExport">' + ico('copy', 14) + '导出驱动</button>'
    +   '<button class="drv-btn" data-drv-add id="drvAdd">' + ico('folder', 14) + '添加驱动</button>'
    + '</div>';
}

function drvSysOptions(){
  if(!DRV_S.systems.length){
    return '<option value="online">当前系统</option>';
  }
  return DRV_S.systems.map(function(s){
    return '<option value="' + drvEsc(s.id) + '"' + (s.id === DRV_S.sys ? ' selected' : '') + '>' + drvEsc(s.name) + '</option>';
  }).join('');
}

function drvEmptyHtml(){
  if(DRV_S.loading){
    return '<div class="drv-empty">' + ico('refresh', 26)
      + '<div class="t">正在枚举驱动存储…</div>'
      + '<div>第三方驱动走 pnputil 枚举；勾上「显示内置驱动」会连系统自带的一起列出。</div></div>';
  }
  if(DRV_S.err){
    return '<div class="drv-empty err">' + ico('warn', 26)
      + '<div class="t">没能读到驱动列表</div><div>' + drvEsc(DRV_S.err) + '</div>'
      + '<div style="margin-top:6px"><button class="drv-btn" data-drv-reload>' + ico('refresh', 14) + '重试</button></div></div>';
  }
  return '<div class="drv-empty">' + ico('search', 26)
    + '<div class="t">' + (drvIsOffline() ? '未读取到该系统驱动' : '没有读到驱动') + '</div>'
    + '<div>勾上「显示内置驱动」可以连系统自带的那批一起看。</div></div>';
}

/* ── 数据 ───────────────────────────────────────────────────────────────── */
function drvLoadSystems(){
  if(!hasGo()) return Promise.resolve();
  return callGo('GetOSSystems').then(function(list){
    DRV_S.systems = (list && list.length) ? list : [{id:'online', name:'当前系统', path:'', online:true}];
    var sel = drvEl('drvSys');
    if(sel) sel.innerHTML = drvSysOptions();
    var hint = drvEl('drvSysHint');
    if(hint) hint.textContent = DRV_S.systems.length > 1 ? ('检测到 ' + DRV_S.systems.length + ' 个 Windows，可切换') : '';
  }).catch(function(){ DRV_S.systems = [{id:'online', name:'当前系统', path:'', online:true}]; });
}

/* 预热（主文件启动时把各页加载器跑一遍）只调 drvLoad，不调 drvAfterPaint，
   所以系统下拉也必须由 drvLoad 自己带出来 —— 否则进页看到的永远只有「当前系统」一项。 */
function drvLoad(){
  if(!hasGo()) return Promise.resolve();
  if(DRV_S.systems.length) return drvLoadList();
  return drvLoadSystems().then(function(){ return drvLoadList(); });
}
function drvLoadList(){
  if(!hasGo()) return Promise.resolve();
  if(DRV_S.loading) return Promise.resolve();
  DRV_S.loading = true; DRV_S.err = '';
  var tree = drvEl('drvTree');
  if(tree) tree.innerHTML = drvEmptyHtml();
  return callGo('GetDrivers', DRV_S.sys, DRV_S.showAll).then(function(list){
    DRV_S.all = list || [];
    DRV_S.loading = false; DRV_S.loaded = true;
    drvRender();
  }).catch(function(e){
    /* 读失败要保留"失败"语义：不能拿空列表当"本来没有"（启动项模块踩过，
       计划任务读不出来被显示成「计划任务 0」，用户以为自己机器上没有）。 */
    DRV_S.loading = false;
    DRV_S.err = String(e && e.message ? e.message : e);
    if(typeof cur !== 'undefined' && cur === 'driver') drvRender();
  });
}

/* 类别数（hero 的「设备类别」）—— 与树里显示的分组口径完全一致 */
function drvCatCount(list){
  var seen = {}, n = 0;
  for(var i = 0; i < list.length; i++){
    var c = list[i].class || '未分类';
    if(!seen[c]){ seen[c] = 1; n++; }
  }
  return n;
}

function drvRender(){
  var tree = drvEl('drvTree');
  var hint = drvEl('drvSysHint');
  if(hint) hint.textContent = DRV_S.systems.length > 1 ? ('检测到 ' + DRV_S.systems.length + ' 个 Windows，可切换') : '';
  /* hero 的数字：读失败写「—」，绝不写 0（0 是"确实没有"，不是"没读到"） */
  var kAll = drvEl('drvKAll'), kCat = drvEl('drvKCats');
  if(kAll) kAll.textContent = DRV_S.err ? '—' : (DRV_S.all.length || 0);
  if(kCat) kCat.textContent = DRV_S.err ? '—' : drvCatCount(DRV_S.all);
  if(!tree) return;
  /* 读失败优先于"上一次的数据"：重新枚举点下去没反应比列表旧更让人困惑。
     数据留在 DRV_S.all 里不清，重试成功后原样回来。 */
  if(DRV_S.err || !DRV_S.all.length){
    tree.innerHTML = drvEmptyHtml();
    drvSelInfo();
    return;
  }

  /* 按设备类别分组（老版本同款：类别数多的排前面） */
  var byCat = {}, order = [];
  DRV_S.all.forEach(function(d){
    var c = d.class || '未分类';
    if(!byCat[c]){ byCat[c] = []; order.push(c); }
    byCat[c].push(d);
  });
  order.sort(function(x, y){ return byCat[y].length - byCat[x].length; });

  tree.innerHTML = order.map(function(c){
    /* 默认全部收起：只列类别，驱动要手动点开才看（用户口径：别自作主张替他展开） */
    var n = byCat[c].length;
    return '<div class="drv-node" data-node="' + drvEsc(c) + '">'
      + '<div class="drv-node-h" data-drv-toggle="1">'
      +   '<span class="drv-caret">' + ico('arrow', 12) + '</span>'
      +   '<input type="checkbox" data-cat="' + drvEsc(c) + '">'
      +   '<span class="drv-cat">' + drvEsc(c) + '</span><i>' + n + '</i>'
      + '</div>'
      + '<div class="drv-kids">' + byCat[c].map(function(d){ return drvLineHtml(d, c); }).join('') + '</div>'
      + '</div>';
  }).join('');
  drvSelInfo();
}

function drvLineHtml(d, cat){
  return '<div class="drv-line">'
    + '<input type="checkbox" data-oem="' + drvEsc(d.oem || '') + '" data-name="' + drvEsc(d.name || d.oem || '') + '"'
    +   ' data-cat="' + drvEsc(cat) + '" data-folder="' + drvEsc(d.folder || '') + '" data-path="' + drvEsc(d.path || '') + '">'
    + '<span class="nm">' + drvEsc(d.name || d.oem || '（未命名）') + '</span>'
    + '<span class="prov">' + drvEsc(d.provider || '未知厂商') + '</span>'
    + '<span class="sp"></span>'
    + (d.version ? '<span class="drv-chip">' + drvEsc(d.version) + '</span>' : '')
    + (d.date ? '<span class="drv-chip">' + drvEsc(d.date) + '</span>' : '')
    + '<span class="drv-chip oem">' + drvEsc(d.oem || '—') + '</span>'
    + (d.builtin ? '<span class="drv-chip in">内置</span>' : '')
    + '</div>';
}

/* 勾选统计：hero 里「已勾选」跟着变（有勾选就高亮），底部「全选」按钮文案切成「取消全选」 */
function drvSelInfo(){
  var n = 0, can = 0;
  drvEach('#drvTree input[data-oem]', function(b){
    if(b.checked) n++;
    can++;
  });
  var kb = drvEl('drvKSel');
  if(kb) kb.textContent = n;
  var kbox = drvEl('drvKSelBox');
  if(kbox) kbox.className = 'drv-kpi' + (n ? ' on' : '');
  var btn = drvEl('drvSelAll');
  if(btn) btn.textContent = (can && n >= can) ? '取消全选' : '全选';
}

/* ── 导出进度：后端 drv:progress（done, total, name）─────────────────────── */
var DRV_PROG_BOUND = false;
function drvBindProgress(){
  if(DRV_PROG_BOUND) return;
  if(!(window.runtime && typeof runtime.EventsOn === 'function')) return;
  DRV_PROG_BOUND = true;
  runtime.EventsOn('drv:progress', function(done, total, name){
    drvProgUpdate(done, total, name);
  });
}
function drvProgShow(on, txt){
  var box = drvEl('drvProg');
  if(!box) return;
  box.className = 'drv-prog' + (on ? ' on' : '');
  if(txt){ var t = drvEl('drvProgTxt'); if(t) t.textContent = txt; }
}
function drvProgUpdate(done, total, name){
  var bar = drvEl('drvProgBar');
  if(!bar) return;
  if(total > 1){
    bar.className = '';
    bar.style.width = Math.min(100, Math.round(done / total * 100)) + '%';
    drvProgShow(true, '正在导出 ' + done + '/' + total + '：' + (name || '') + '（请勿关闭）');
  }else if(total === 1){
    /* 全量导出只有一个阶段，用流动条表示"在进行中"，不编百分比 */
    bar.className = 'indet';
    drvProgShow(true, name || '正在导出全部驱动，请稍候…');
  }else{
    drvProgShow(false);
  }
}
function drvBusy(on){
  DRV_S.busy = !!on;
  drvEach('#drvExport,#drvAdd,#drvReload', function(b){ b.disabled = !!on; });
}

/* ── 导出 ───────────────────────────────────────────────────────────────── */
function drvPicked(){
  var out = [];
  drvEach('#drvTree input[data-oem]', function(b){
    if(!b.checked) return;
    var oem = b.getAttribute('data-oem') || '';
    var folder = b.getAttribute('data-folder') || '';
    var path = b.getAttribute('data-path') || '';
    if(!oem && !folder && !path) return;
    out.push({oem:oem, name:b.getAttribute('data-name') || oem, cat:b.getAttribute('data-cat') || '',
      folder:folder, path:path});
  });
  return out;
}

function drvExport(){
  if(DRV_S.busy) return;
  drvBindProgress();
  var picked = drvPicked();
  var off = drvIsOffline();
  var go = function(){
    callGo('SelectFolder', '选择驱动备份保存位置', '').then(function(dir){
      if(!dir){ toast('已取消（未选择保存位置）', 'info'); return; }
      drvBusy(true);
      drvProgUpdate(0, 1, '正在准备导出…');
      var p = picked.length
        ? callGo('ExportDrivers', picked, DRV_S.sys, dir)
        : callGo('ExportAllDrivers', DRV_S.sys, dir);
      return p.then(function(msg){
        drvProgShow(false); drvBusy(false);
        if(msg){ toast(msg, 'warn'); return; }
        toast('导出完成：已按「类别 / 驱动名」分目录保存\n' + dir, 'ok');
      });
    }).catch(function(e){
      drvProgShow(false); drvBusy(false);
      toast('导出失败：' + e, 'danger');
    });
  };
  /* 没勾选 = 导出该系统全部驱动（老版本的习惯：不勾就是全部），先问一句再跑 */
  if(!picked.length){
    drvConfirm('未勾选驱动，导出全部？',
      '你没有勾选任何驱动，将导出' + (off ? '该系统' : '本机') + '全部驱动。耗时约 1–3 分钟，下一步可以选择保存位置。',
      '继续', go);
    return;
  }
  go();
}

function drvAdd(){
  if(DRV_S.busy) return;
  callGo('SelectFolder', '选择包含驱动（.inf）的文件夹', '').then(function(dir){
    if(!dir) return;
    drvBusy(true);
    toast('正在安装驱动（递归文件夹内所有 .inf）…', 'info');
    return callGo('AddDriver', dir).then(function(msg){
      drvBusy(false);
      if(msg){ toast(msg, 'warn'); return; }
      toast('驱动安装完成，列表已刷新', 'ok');
      drvLoad();
    });
  }).catch(function(e){
    drvBusy(false);
    toast('安装失败：' + e, 'danger');
  });
}

/* 打开设备管理器：复用工具库里已有的「快速打开系统工具」，不自造命令 */
function drvDevMgr(){
  callGo('RunTool', 'util-opentool', 'devmgmt').then(function(err){
    if(err) toast('打开失败：' + err, 'warn');
    else toast('已打开设备管理器', 'ok');
  }).catch(function(e){ toast('打开失败：' + e, 'warn'); });
}

/* ── 确认弹窗（v1.1 的 #modal + #modalBox + .open，不是 v2 的 modal(html)）── */
function drvConfirm(title, body, okText, onOk){
  var box = drvEl('modalBox'), mod = drvEl('modal');
  if(!box || !mod || typeof closeModal !== 'function'){ onOk(); return; }
  box.innerHTML = '<div class="modal-t">' + drvEsc(title) + '</div>'
    + '<div class="modal-d">' + drvEsc(body) + '</div>'
    + '<div class="modal-f"><button class="btn" id="drvCancel">取消</button>'
    + '<button class="btn primary" id="drvOk">' + ico('check', 16) + drvEsc(okText || '确定') + '</button></div>';
  mod.classList.add('open');
  var c = drvEl('drvCancel'), o = drvEl('drvOk');
  if(c) c.onclick = closeModal;
  if(o) o.onclick = function(){ closeModal(); onOk(); };
}

/* ── 事件：整页委托挂 #content（只换 innerHTML，元素永不重建）──────────── */
function drvClick(e){
  var t = e.target;
  /* 勾选框自己走 change，别顺带把类别展开/收起 */
  while(t && t !== e.currentTarget){
    if(t.tagName === 'INPUT') return;                     /* 勾选框自己走 change */
    /* 「设备管理器」是真链接，要在把 <a> 一律放行之前先接住 */
    if(t.hasAttribute && t.hasAttribute('data-drv-devmgr')){ e.preventDefault(); drvDevMgr(); return; }
    if(t.tagName === 'SELECT' || t.tagName === 'OPTION' || t.tagName === 'A') return;
    if(t.hasAttribute && t.hasAttribute('data-drv-toggle')){
      var node = drvNodeOf(t);
      if(node){ node.className = String(node.className).indexOf('open') >= 0 ? 'drv-node' : 'drv-node open'; }
      return;
    }
    if(t.hasAttribute){
      if(t.hasAttribute('data-drv-selall')){ drvSelAll(); return; }
      if(t.hasAttribute('data-drv-reload')){ toast('正在重新枚举…', 'info'); drvLoad(); return; }
      if(t.hasAttribute('data-drv-export')){ drvExport(); return; }
      if(t.hasAttribute('data-drv-add')){ drvAdd(); return; }
      if(t.hasAttribute('data-drv-devmgr')){ e.preventDefault(); drvDevMgr(); return; }
    }
    t = t.parentNode;
  }
}
function drvChange(e){
  var t = e.target;
  if(!t || (t.tagName !== 'INPUT' && t.tagName !== 'SELECT')) return;
  /* 系统下拉是 <select>，不是 <input> —— 早期这里只放 INPUT 通过，切系统看着毫无反应 */
  if(t.id === 'drvSys'){ DRV_S.sys = t.value; drvLoad(); return; }
  if(t.id === 'drvShowAll'){ DRV_S.showAll = t.checked; drvLoad(); return; }
  if(t.hasAttribute('data-cat')){
    /* 类别框 → 同步该类别下所有驱动框（两边永远一致）
       ⚠️ 找父节点必须按完整类名匹配：'drv-node-h' 里也含 'drv-node' 这段字符串，
       用 indexOf 会停在标题行上，querySelectorAll 查不到子项 → 类别勾选看着没反应。 */
    var node = drvNodeOf(t);
    if(node){
      var kids = node.querySelectorAll('input[data-oem]');
      for(var i = 0; i < kids.length; i++) kids[i].checked = t.checked;
    }
    drvSelInfo();
    return;
  }
  if(t.hasAttribute('data-oem')){ drvSelInfo(); return; }
}
/* 从树里的任意元素上溯到它所属的类别节点（.drv-node，不是 .drv-node-h） */
function drvNodeOf(el){
  var n = el;
  while(n && n !== document){
    if(n.classList && n.classList.contains('drv-node')) return n;
    n = n.parentNode;
  }
  return null;
}
function drvSelAll(){
  var boxes = document.querySelectorAll('#drvTree input[data-oem]');
  if(!boxes.length){ toast('当前列表里没有可勾选的驱动', 'warn'); return; }
  var allOn = true;
  for(var i = 0; i < boxes.length; i++){ if(!boxes[i].checked){ allOn = false; break; } }
  for(var j = 0; j < boxes.length; j++) boxes[j].checked = !allOn;
  var cats = document.querySelectorAll('#drvTree .drv-node-h input[data-cat]');
  for(var k = 0; k < cats.length; k++) cats[k].checked = !allOn;
  drvSelInfo();
  toast(allOn ? '已取消全选' : ('已全选 ' + boxes.length + ' 个驱动'), 'ok');
}
/* ── 绑定与进页补画 ─────────────────────────────────────────────────────── */
function bindDriver(){
  var root = drvEl('content');
  if(!root) return;
  if(root.getAttribute('data-drvBound') !== '1'){
    root.setAttribute('data-drvBound', '1');
    root.addEventListener('click', drvClick);
    root.addEventListener('change', drvChange);
  }
  drvAfterPaint();
}
function drvAfterPaint(){
  drvBindProgress();
  if(!DRV_S.loaded && !DRV_S.loading){
    /* 系统列表只取一次（换系统靠下拉，不用重取）；取完再拉驱动 */
    drvLoadSystems().then(function(){ drvLoad(); });
    return;
  }
  if(DRV_S.loaded) drvRender();
}

window.renderDriver = renderDriver;
window.bindDriver   = bindDriver;
window.drvLoad      = drvLoad;
window.DRV_S        = DRV_S;
