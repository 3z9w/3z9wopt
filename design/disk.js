/* ============================================================================
   disk.js —— 磁盘空间页（2026-10-11 从老版本 100% 移植 + 独立成模块）
   ---------------------------------------------------------------------------
   载入方式与 pcinfo.js / opt.js / uninst.js 完全一致（见主文件 <head> 里的
   「磁盘空间页独立文件加载器」）：主文件用**同步 XHR** 读本文件再**直接 eval**，
   这样 window.renderDisk / window.bindDisk 才在 PAGES、reloadCurrentPage
   引用它们之前就位。

   功能是老版本（v2）成熟实现的**逐行移植**，行为/判定/参数一律照搬：
     · 盘列表      GetDiskList（本版接口，带卷标名与固态标记，首页也用它）
     · 扫描        SpaceScanAsync（MFT 直读），进度/完成走 space:progress / space:done
     · 目录分析    SpaceChildren / SpaceCrumbs —— 列表 + 矩形图两种视图
     · 大文件      SpaceTopFiles(minSize, limit)
     · 重复文件    SpaceDupScanAsync —— 同大小分组 → 抽样哈希 → 全量 MD5 确认
     · 删除        SpaceDelete —— 一律进**回收站**，误删可还原
     · 定位        SpaceOpen —— 资源管理器选中该文件

   与老版本唯一的两处差别（用户 2026-10-11 定的）：
     ① 目录分析**默认显示矩形图**（老版本默认列表；两个视图都还在，随时切）
     ② 按钮、配色换成 1.1 的视觉语言（--sp-grad 渐变 / 玻璃卡 / 主题变量）

   ⚠️ 交互一律**事件委托挂 #content**（项目铁律：整页重绘会吞点击）：
      扫描、切 Tab、钻取、排序、切视图、删除都只换 #spDrives / #spStats / #spBody
      三个容器里的内容，不整页 render()，所以不存在 mousedown/mouseup 落在不同
      节点上导致 click 不派发的问题。
   ============================================================================ */

window.DK = window.DK || {
  drives: [], drive: '', scanning: false, pct: 0, ready: false, err: '',
  tab: 'dir', frn: 5, crumbs: [], rows: [], showN: 300, sortBy: 'size', sortDir: 1,
  view: 'map',                  /* 目录分析的默认视图：矩形图（老版本默认列表，用户要求改） */
  topRows: [], topMin: 1073741824,
  dupGroups: [], dupFreed: 0, dupScanning: false, dupPct: 0, dupStage: '', dupMin: 104857600,
  stats: {}, dirLoaded: false, evtBound: false
};
var DK = window.DK;

/* ── 小工具 ─────────────────────────────────────────────────────────────── */
function dkEsc(s){
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
/* MFT 里的 mtime 是 FILETIME（100ns，1601 起）→ 日期串；0 就是「没取到」，显示 — */
function dkTime(ft){
  var ms = ft > 0 ? ft / 10000 - 11644473600000 : 0;
  return ms > 0 ? new Date(ms).toLocaleDateString('sv-SE') : '—';
}
function dkEl(id){ return document.getElementById(id); }
function dkEach(sel, fn){
  var l = document.querySelectorAll(sel);
  for(var i = 0; i < l.length; i++) fn(l[i], i);
}

/* ── 新手说明：这一项是什么 / 能不能删 / 有什么影响（悬停看，原样移植）────── */
function dkHint(name, path, isDir){
  var n = (name || '').toLowerCase();
  var p = ((path || '').toLowerCase()) + (n ? '\\' + n : '');
  /* 顺序即优先级：越靠前越优先命中 */
  var R = [
    [/pagefile\.sys$/, '禁删', 'red', '虚拟内存页面文件：Windows 用它当「内存扩展」。删除后会自动重建，想调整大小请到「系统优化」改虚拟内存，不要手动删。'],
    [/hiberfil\.sys$/, '禁删', 'red', '休眠文件：保存「休眠 / 快速启动」时的内存快照。手动删会失去休眠功能，释放空间请用 powercfg 命令关闭休眠。'],
    [/swapfile\.sys$/, '禁删', 'red', 'UWP 应用专用交换文件，由系统自动管理。'],
    [/\\windows\.old$/, '可删', 'grn', '旧系统备份：升级 / 重装前的完整系统。确认新系统一切正常后可以删（建议用「垃圾清理」里的 Windows.old 项，更安全）。'],
    [/\$recycle\.bin$/, '可清', 'grn', '回收站的实际存储位置。清空请用「垃圾清理」里的「回收站」项。'],
    [/\\system volume information$/, '禁删', 'red', '系统还原点 / 卷影副本的存储区，删除后系统无法回滚。'],
    [/\\temp$/, '可清', 'grn', '临时文件目录：程序运行产生的中间文件，可安全清空（正在使用的文件会自动跳过）。'],
    [/\\(cache|caches|code cache|gpucache|shadercache|cachestorage)$/, '可清', 'grn', '缓存目录：加速用的临时数据，删除后下次打开自动重建，不影响功能与个人数据。'],
    [/\\windows(\\|$)/, '禁删', 'red', 'Windows 系统目录，存放系统文件，删除其中任何内容都可能导致系统故障。'],
    [/\\program files/, '勿手删', 'amb', '软件安装目录：每个子文件夹对应一个软件。卸载请用「应用卸载」，直接删文件夹会留下注册表残留。'],
    [/\\programdata(\\|$)/, '勿手删', 'amb', '软件公共数据目录（配置 / 许可证 / 缓存），删除可能导致软件异常。'],
    [/\\appdata/, '勿手删', 'amb', '应用数据目录：软件的配置和缓存都在这里，也是卸载残留的高发区。'],
    [/\\node_modules$/, '可删', 'amb', '项目第三方依赖包，删除后需要重新安装依赖项目才能运行。'],
    [/\\users(\\|$)/, '一般', '', '用户数据根目录：桌面、文档、下载都在其中的子文件夹里。'],
    [/^downloads$/, '一般', '', '下载目录：浏览器和下载工具的默认保存位置，删除前确认没有还要的文件（会进回收站，可还原）。'],
    [/\.dll$/, '勿手删', 'red', '程序组件库：运行时被程序加载，删除会导致对应软件报错。'],
    [/\.sys$/, '禁删', 'red', '驱动 / 系统内核文件，删除可能导致蓝屏或无法开机。'],
    [/\.exe$/, '勿手删', 'amb', '可执行程序：卸载软件请用「应用卸载」，直接删除会留下残留。'],
    [/\.lnk$/, '一般', '', '快捷方式：只是软件的「入口」，删除不影响软件本体。']
  ];
  for(var i = 0; i < R.length; i++){
    if(R[i][0].test(p)) return { tag: R[i][1], cls: R[i][2], txt: R[i][3] };
  }
  return isDir
    ? { tag: '', cls: '', txt: '普通目录：删除会连带删除其中的全部内容，删除前建议点进去看一眼（删除进回收站，可还原）。' }
    : { tag: '', cls: '', txt: '普通文件：删除前确认不再需要（删除进回收站，可还原）。' };
}
function dkTag(h){ return h.cls ? '<i class="sp-tag t-' + h.cls + '">' + h.tag + '</i>' : ''; }

function dkRowHtml(r, max){
  var pct = Math.max(1, Math.round(r.size * 100 / max));
  var dirPath = (DK.crumbs || []).slice(1).map(function(c){ return c.name; }).filter(Boolean).join('\\');
  var h = dkHint(r.name, dirPath, r.isDir);
  return '<div class="sp-row sp-cols" data-frn-row="' + r.frn + '">'
    + '<span class="sp-name">' + ico(r.isDir ? 'folder' : 'file', 17)
    +   '<span class="sp-nm' + (r.isDir ? ' isdir' : '') + '"' + (r.isDir ? ' data-frn="' + r.frn + '"' : '')
    +   ' title="' + dkEsc(h.txt) + (r.isDir ? '\n点击进入查看明细' : '') + '">' + dkEsc(r.name) + '</span>' + dkTag(h) + '</span>'
    + '<span class="sp-bar"><span class="' + (r.isDir ? '' : 'file') + '" style="width:' + pct + '%"></span></span>'
    + '<span class="num sz">' + fmtSize(r.size)
    +   (r.isDir ? '<i>' + (r.files ? r.files.toLocaleString() + ' 个文件' : '') + '</i>' : '') + '</span>'
    + '<span class="num tm">' + dkTime(r.mtime) + '</span>'
    + '<span class="num acts">'
    +   '<button class="sp-act" data-open="' + r.frn + '" title="在资源管理器中显示所在位置">' + ico('search', 15) + '</button>'
    +   '<button class="sp-act danger" data-del="' + r.frn + '" title="删除到回收站（可还原）">' + ico('trash', 15) + '</button>'
    + '</span></div>';
}

/* ── 页面骨架 ───────────────────────────────────────────────────────────── */
window.renderDisk = function(){
  var tabs = [['dir', 'folder', '目录分析'], ['top', 'file', '大文件'], ['dup', 'copy', '重复文件']];
  return '<div class="page-head" style="display:flex;align-items:flex-end;justify-content:space-between;gap:14px">'
    + '<div><div class="page-title">磁盘空间</div>'
    + '<div class="page-sub" id="spSub">正在读取磁盘列表…</div></div>'
    + '<button class="btn sm primary" id="spScan"' + (DK.scanning ? ' disabled' : '') + '>'
    +   ico('refresh', 14) + '开始扫描</button></div>'
    + '<div class="sp-drives" id="spDrives"></div>'
    + '<div class="sp-stats" id="spStats"></div>'
    + '<div class="sp-seg" id="spSeg">'
    +   tabs.map(function(t){
          /* 不在 Tab 上放"有多少组"的角标：Tab 只在整页渲染时画一次，扫描完它不会更新，
             看着像坏数据；组数本来就在上面的概览卡里（重复文件组 N 组）。 */
          return '<button class="sp-tab" data-dtab="' + t[0] + '">' + ico(t[1], 15) + t[2] + '</button>';
        }).join('')
    + '</div>'
    + '<div class="sp-body" id="spBody"></div>';
};

/* ── 三个容器：只换它们的内容，不整页重绘 ───────────────────────────────── */
function dkPaint(){
  dkRenderDrives();
  dkRenderStats();
  dkRenderBody();
  dkEach('#spSeg .sp-tab', function(b){
    b.classList.toggle('on', b.getAttribute('data-dtab') === DK.tab);
  });
  var sub = dkEl('spSub');
  if(sub){
    sub.innerHTML = DK.ready
      ? 'MFT 直读索引 · <b>' + dkEsc(DK.stats.drive || DK.drive) + '</b> 盘 · '
        + (DK.stats.files || 0).toLocaleString() + ' 个文件，三个视图共用一次扫描'
      : 'MFT 直读引擎：百万文件全盘索引只需数秒（需管理员权限，仅支持 NTFS 盘）。'
        + '删文件一律进<b>回收站</b>，误删可还原。';
  }
  var scan = dkEl('spScan');
  if(scan) scan.disabled = !!DK.scanning;
}

function dkRenderDrives(){
  var host = dkEl('spDrives');
  if(!host) return;
  host.innerHTML = DK.drives.map(function(d){
    var used = d.total - d.free, pct = d.total ? Math.round(used * 100 / d.total) : 0;
    return '<button class="sp-drive' + (d.drive === DK.drive ? ' on' : '') + (d.ntfs ? '' : ' off') + '"'
      + ' data-drive="' + dkEsc(d.drive) + '"' + (d.ntfs ? '' : ' disabled title="仅支持 NTFS 磁盘"') + '>'
      + '<div class="sp-drive-top"><b>' + dkEsc(d.drive) + '</b>'
      +   '<span>' + dkEsc(d.label || d.name || (d.ntfs ? '本地磁盘' : '非 NTFS')) + '</span><i>' + pct + '%</i></div>'
      + '<div class="sp-drive-bar"><span style="width:' + pct + '%"></span></div>'
      + '<div class="sp-drive-sub">' + fmtSize(used) + ' / ' + fmtSize(d.total)
      +   (d.drive === DK.drive && DK.scanning ? ' · 扫描中…' : '') + '</div></button>';
  }).join('');
}

function dkRenderStats(){
  var host = dkEl('spStats');
  if(!host) return;
  if(!DK.ready){ host.innerHTML = ''; return; }
  var st = DK.stats || {};
  var drv = DK.drives.filter(function(x){ return x.drive === DK.drive; })[0] || {};
  var cards = [
    ['已用空间', fmtSize(st.bytes || (drv.total - drv.free) || 0), ''],
    ['文件总数', (st.files || 0).toLocaleString(), ''],
    ['重复文件组', st.dupGroups ? st.dupGroups + ' 组' : '待分析', st.dupGroups ? '点「重复文件」查看' : ''],
    ['重复可释放', st.dupFree ? fmtSize(st.dupFree) : '待分析', st.dupFree ? '同一份文件存了多份' : '']
  ];
  host.innerHTML = cards.map(function(c){
    return '<div class="sp-card"><div class="l">' + c[0] + '</div><div class="v">' + c[1] + '</div>'
      + (c[2] ? '<div class="s">' + c[2] + '</div>' : '') + '</div>';
  }).join('');
}

function dkRenderBody(){
  var host = dkEl('spBody');
  if(!host) return;
  if(DK.scanning){ dkScanning(host); return; }
  if(DK.err){
    host.innerHTML = '<div class="card"><div class="empty sp-empty">'
      + '<div class="empty-ico" style="color:var(--danger)">' + ico('warn', 30) + '</div>'
      + '<div class="empty-t">无法扫描</div>'
      + '<div class="empty-d">' + dkEsc(DK.err) + '<br>磁盘空间需要管理员权限读取 MFT，'
      + '请以管理员身份重新运行本软件；非 NTFS 的盘（exFAT / FAT32 的 U 盘）不支持。</div></div></div>';
    return;
  }
  if(!DK.ready){
    host.innerHTML = '<div class="card"><div class="empty sp-empty">'
      + '<div class="empty-ico" style="color:var(--info)">' + ico('chart', 30) + '</div>'
      + '<div class="empty-t">' + (DK.drives.length ? '选择一个磁盘开始' : '磁盘列表未获取到') + '</div>'
      + '<div class="empty-d">' + (DK.drives.length
          ? '点上方磁盘卡即可扫描。扫描直接读取 NTFS 主文件表（MFT），不逐个文件遍历，全盘百万文件只需几秒。'
          : '后端没连上时这里读不到盘（浏览器直开设计预览就是这样）。') + '</div></div></div>';
    return;
  }
  if(DK.tab === 'dir'){ dkRenderDir(); return; }
  if(DK.tab === 'top'){ dkRenderTop(); return; }
  dkRenderDup();
}

function dkScanning(host){
  host.innerHTML = '<div class="card sp-empty">'
    + '<div class="sp-scan-ring" style="--p:' + DK.pct + '"><span id="spPctText">' + DK.pct + '%</span></div>'
    + '<h3>正在读取 ' + dkEsc(DK.drive) + ' 盘主文件表…</h3>'
    + '<p>全盘索引通常只需几秒，视文件数量而定。</p>'
    + '<div class="sp-pct-bar"><span id="spPctBar" style="width:' + DK.pct + '%"></span></div></div>';
}

/* ── 目录分析 ───────────────────────────────────────────────────────────── */
function dkSorted(){
  var k = DK.sortBy, dir = DK.sortDir;
  return DK.rows.slice().sort(function(a, b){
    if(k === 'name') return dir * String(a.name).localeCompare(String(b.name), 'zh-Hans-CN');
    if(k === 'mtime') return dir * (b.mtime - a.mtime);
    return dir * (b.size - a.size);
  });
}

function dkDrill(frn){
  DK.frn = frn;
  Promise.all([callGo('SpaceChildren', frn), callGo('SpaceCrumbs', frn)]).then(function(r){
    DK.rows = r[0] || [];
    DK.crumbs = r[1] || [];
    DK.showN = 300;
    dkRenderDir();
  }).catch(function(e){ toast('读取目录失败：' + (e && e.message ? e.message : e), 'danger'); });
}

function dkRenderDir(){
  var host = dkEl('spBody');
  if(!host) return;
  if(!DK.rows.length && !DK.dirLoaded){
    DK.dirLoaded = true;
    dkDrill(DK.frn);
    return;
  }
  var rows = dkSorted().filter(function(r){ return r.size > 0; });
  var shown = rows.slice(0, DK.showN);
  var max = rows.length ? rows[0].size : 1;
  var crumb = DK.crumbs.map(function(c, i){
    return '<button class="sp-crumb' + (i === DK.crumbs.length - 1 ? ' cur' : '') + '" data-frn="' + c.frn + '">'
      + dkEsc(c.name) + '</button>';
  }).join('<span class="sp-crumb-sep">›</span>');

  /* 面包屑第一项就是盘根（C:），不再单独放一个"回根目录"图标 —— 重复且占地方 */
  var head = '<div class="sp-crumb-bar">'
    + crumb
    + '<span style="flex:1"></span>'
    + '<div class="sp-viewseg">'
    +   '<button class="' + (DK.view === 'list' ? 'on' : '') + '" data-view="list">列表</button>'
    +   '<button class="' + (DK.view === 'map' ? 'on' : '') + '" data-view="map">矩形图</button>'
    + '</div></div>';

  host.innerHTML = head + (DK.view === 'list' ? dkListHtml(rows, shown, max) : dkMapHtml());
  if(DK.view === 'map') dkDrawMap(rows, dkEl('spMap'));
}

function dkListHtml(rows, shown, max){
  return '<div class="sp-tbl">'
    + '<div class="sp-thead sp-cols">'
    +   '<span data-sort="name" class="' + (DK.sortBy === 'name' ? 'on' : '') + '">名称</span><span>占比</span>'
    +   '<span class="num ' + (DK.sortBy === 'size' ? 'on' : '') + '" data-sort="size">大小</span>'
    +   '<span class="num ' + (DK.sortBy === 'mtime' ? 'on' : '') + '" data-sort="mtime">修改时间</span>'
    +   '<span class="num">操作</span></div>'
    + '<div id="spRows">' + shown.map(function(r){ return dkRowHtml(r, max); }).join('') + '</div>'
    + (rows.length > DK.showN ? '<button class="sp-more" id="spMore">显示更多（还有 '
        + (rows.length - DK.showN).toLocaleString() + ' 项）</button>' : '')
    + (rows.length ? '' : '<div class="sp-norow">此目录为空</div>')
    + '</div>';
}
function dkMapHtml(){
  return '<div class="card sp-mapcard"><canvas id="spMap" width="1200" height="560"></canvas>'
    + '<div class="sp-map-hint">矩形面积 = 占用空间，单击钻取，点面包屑返回上一层</div></div>';
}

/* ── 矩形图（squarified treemap，算法原样移植）───────────────────────────── */
function dkDrawMap(rows, canvas){
  if(!canvas) return;
  var ctx = canvas.getContext('2d');
  var W = canvas.width, H = canvas.height;
  /* 配色跟页面同源：蓝为主、青作衬托（原来是 8 色虹彩，跟本机配置的「一屏一色」不搭）。
     相邻块靠**明度台阶**区分，不是靠色相乱跳 —— 2026-10-11 用户定。 */
  var palette = ['#1e40af', '#3b82f6', '#0891b2', '#4f8ff7', '#0e7490', '#4f46e5', '#0ea5e9', '#155e75'];
  var items = rows.filter(function(r){ return r.size > 0; })
    .sort(function(a, b){ return b.size - a.size; }).slice(0, 120);
  var total = items.reduce(function(s, r){ return s + r.size; }, 0) || 1;
  var light = document.documentElement.getAttribute('data-theme') === 'light';
  ctx.clearRect(0, 0, W, H);
  var cells = [];
  (function squarify(list, x, y, w, h, ci){
    if(!list.length || w < 10 || h < 10) return;
    if(list.length === 1){ cells.push({ r: list[0], x: x, y: y, w: w, h: h, c: ci }); return; }
    /* ⚠️ 比例必须按**当前这块剩余区域**算，不能用全局 total：
       老版本写的是 scale = w*h/total（total 是全部项之和），递归到第二块之后
       面积越缩越小，图上一大片空白。这里改成"剩余面积 / 剩余大小之和"。 */
    var sum = 0;
    for(var t = 0; t < list.length; t++) sum += list[t].size;
    var scale = w * h / (sum || 1);
    var horizontal = w < h;
    var side = horizontal ? w : h;
    var row = [], rowSum = 0, best = Infinity, i = 0;
    for(; i < list.length; i++){
      var nextSum = rowSum + list[i].size * scale;
      var rowAndNext = row.concat(list[i]);
      var worst = 0;
      for(var k = 0; k < rowAndNext.length; k++){
        var area = rowAndNext[k].size * scale, len = nextSum / side;
        var ratio = Math.max(area / (len * len), (len * len) / area);
        if(ratio > worst) worst = ratio;
      }
      if(worst > best && row.length) break;
      row.push(list[i]); rowSum = nextSum; best = worst;
    }
    var rest = list.slice(row.length);
    if(horizontal){
      var rh = rowSum / w, xx = x;
      row.forEach(function(it, k){
        var cw = it.size * scale / rh;
        cells.push({ r: it, x: xx, y: y, w: cw, h: rh, c: ci + k });
        xx += cw;
      });
      squarify(rest, x, y + rh, w, h - rh, ci + 3);
    } else {
      var rw = rowSum / h, yy = y;
      row.forEach(function(it, k){
        var ch = it.size * scale / rw;
        cells.push({ r: it, x: x, y: yy, w: rw, h: ch, c: ci + k });
        yy += ch;
      });
      /* ⚠️ 老版本这一递归调用是错的（竖排时把剩余区域算成 w × (h-rw)，参数还漏了一个），
         结果整张图只剩第一列，其余项全被丢掉。矩形图现在是**默认视图**，必须修：
         竖排时第一列占掉 rw 宽，剩下的是 (w-rw) 宽 × 整高 h。
         2026-10-11 移植时修掉。 */
      squarify(rest, x + rw, y, w - rw, h, ci + 3);
    }
  })(items, 0, 0, W, H, 0);
  DK.cells = cells;
  cells.forEach(function(c){
    ctx.globalAlpha = .9;
    ctx.fillStyle = palette[c.c % palette.length];
    ctx.fillRect(c.x + 1, c.y + 1, Math.max(1, c.w - 2), Math.max(1, c.h - 2));
    ctx.globalAlpha = 1;
    ctx.strokeStyle = light ? '#ffffff' : 'rgba(8,14,30,.85)';
    ctx.lineWidth = 2;
    ctx.strokeRect(c.x + 1, c.y + 1, Math.max(1, c.w - 2), Math.max(1, c.h - 2));
    if(c.w > 70 && c.h > 30){
      ctx.fillStyle = '#fff';
      ctx.font = '500 15px sans-serif';
      ctx.fillText(c.r.name.slice(0, Math.floor(c.w / 9)), c.x + 10, c.y + 22);
      if(c.h > 52){
        ctx.font = '12px sans-serif';
        ctx.fillText(fmtSize(c.r.size), c.x + 10, c.y + 42);
      }
    }
  });
}

function dkMapHit(canvas, e){
  if(!canvas || !DK.cells) return null;
  var rect = canvas.getBoundingClientRect();
  var x = (e.clientX - rect.left) * (canvas.width / rect.width);
  var y = (e.clientY - rect.top) * (canvas.height / rect.height);
  for(var i = 0; i < DK.cells.length; i++){
    var c = DK.cells[i];
    if(x >= c.x && x <= c.x + c.w && y >= c.y && y <= c.y + c.h) return c;
  }
  return null;
}

/* ── 大文件 ─────────────────────────────────────────────────────────────── */
function dkRenderTop(){
  var host = dkEl('spBody');
  if(!host) return;
  host.innerHTML = '<div class="sp-crumb-bar">'
    + '<div class="sp-minsel">找出大于 <select id="spTopMin">'
    +   [100 << 20, 500 << 20, 1 << 30, 4 << 30].map(function(v){
          return '<option value="' + v + '"' + (v === DK.topMin ? ' selected' : '') + '>' + fmtSize(v) + '</option>';
        }).join('')
    + ' </select>的文件</div><span style="flex:1"></span>'
    + '<button class="btn sm" id="spTopGo">' + ico('refresh', 14) + '刷新</button></div>'
    + '<div class="sp-tbl"><div class="sp-thead sp-cols">'
    +   '<span>文件</span><span></span><span class="num">大小</span>'
    +   '<span class="num">修改时间</span><span class="num">操作</span></div>'
    + '<div id="spTopRows">' + (DK.topRows.length ? '' : '<div class="sp-norow">加载中…</div>') + '</div>';
  dkLoadTop();
}

function dkLoadTop(){
  var sel = dkEl('spTopMin');
  if(sel) DK.topMin = +sel.value;
  var box = dkEl('spTopRows');
  callGo('SpaceTopFiles', DK.topMin, 500).then(function(list){
    DK.topRows = list || [];
    if(!box) return;
    var max = DK.topRows.length ? DK.topRows[0].size : 1;
    box.innerHTML = DK.topRows.length ? DK.topRows.map(function(r){
      var h = dkHint(r.name, r.path, false);
      return '<div class="sp-row sp-cols">'
        + '<span class="sp-name wide" title="' + dkEsc(h.txt) + '">' + ico('file', 16)
        +   '<span class="sp-nm">' + dkEsc(r.name) + '</span>' + dkTag(h)
        +   '<i class="sp-path">' + dkEsc(r.path) + '</i></span>'
        + '<span class="sp-bar"><span class="file" style="width:'
        +   Math.max(1, Math.round(r.size * 100 / max)) + '%"></span></span>'
        + '<span class="num sz">' + fmtSize(r.size) + '</span>'
        + '<span class="num tm">' + dkTime(r.mtime) + '</span>'
        + '<span class="num acts">'
        +   '<button class="sp-act" data-open-path="' + dkEsc(r.path) + '" title="在资源管理器中显示所在位置">' + ico('search', 15) + '</button>'
        +   '<button class="sp-act danger" data-del-path="' + dkEsc(r.path) + '" title="删除到回收站（可还原）">' + ico('trash', 15) + '</button>'
        + '</span></div>';
    }).join('') : '<div class="sp-norow">没有超过 ' + fmtSize(DK.topMin) + ' 的文件</div>';
  }).catch(function(){
    if(box) box.innerHTML = '<div class="sp-norow">读取失败</div>';
  });
}

/* ── 重复文件 ───────────────────────────────────────────────────────────── */
function dkDupProgHtml(){
  return '<div class="sp-dup-prog"><span>' + dkEsc(DK.dupStage) + ' ' + DK.dupPct + '%</span>'
    + '<div class="sp-pct-bar"><span style="width:' + DK.dupPct + '%"></span></div></div>';
}

function dkRenderDup(){
  var host = dkEl('spBody');
  if(!host) return;
  host.innerHTML = '<div class="sp-crumb-bar">'
    + '<div class="sp-minsel">查找同一磁盘上大小超过 <select id="spDupMin">'
    +   [10 << 20, 100 << 20, 1 << 30].map(function(v){
          return '<option value="' + v + '"' + (v === DK.dupMin ? ' selected' : '') + '>' + fmtSize(v) + '</option>';
        }).join('')
    + ' </select>的重复文件</div><span style="flex:1"></span>'
    + '<button class="btn sm primary" id="spDupGo"' + (DK.dupScanning ? ' disabled' : '') + '>'
    +   ico('copy', 14) + '开始查找</button></div>'
    + (DK.dupScanning ? '<div class="card sp-dupcard">' + dkDupProgHtml()
        + '<p class="sp-hint2">先按大小分组，再对疑似文件抽样 + 全量哈希确认，零误判。</p></div>' : '')
    + '<div id="spDupList"></div>';

  var list = dkEl('spDupList');
  if(!list) return;
  if(!DK.dupGroups.length && !DK.dupScanning){
    list.innerHTML = '<div class="card"><div class="empty sp-empty">'
      + '<div class="empty-ico" style="color:var(--info)">' + ico('copy', 30) + '</div>'
      + '<div class="empty-t">还没有分析结果</div>'
      + '<div class="empty-d">重复文件 = 同一磁盘上内容完全相同的文件（哈希确认）。'
      + '删除多余副本后，留下一份原文件。</div></div></div>';
    return;
  }
  if(DK.dupFreed) dkRenderStats();
  list.innerHTML = '<div class="sp-dup-sum">发现 <b>' + DK.dupGroups.length + '</b> 组重复，'
    + '最多可释放 <b>' + fmtSize(DK.dupFreed) + '</b>。'
    + '<span class="sp-hint2">每组默认保留第一份（路径最短的），勾选要删除的多余副本。</span></div>'
    + DK.dupGroups.map(function(g, gi){
        return '<div class="card sp-dupgroup" data-gi="' + gi + '">'
          + '<div class="sp-dupg-head"><span class="sz">' + fmtSize(g.size) + ' × ' + g.paths.length + ' 份</span>'
          + '<span class="freed">删多余可释放 ' + fmtSize(g.size * (g.paths.length - 1)) + '</span>'
          + '<span style="flex:1"></span>'
          + '<button class="btn sm danger sp-dup-del" data-gi="' + gi + '">'
          +   ico('trash', 14) + '删除勾选副本</button></div>'
          + g.paths.map(function(p, pi){
              var nm = p.replace(/^.*[\\/]/, '');
              var h = dkHint(nm, p, false);
              return '<label class="sp-duppath' + (pi === 0 ? ' keep' : '') + '">'
                + '<input type="checkbox" data-dup-path="' + dkEsc(p) + '"'
                +   (pi === 0 ? ' disabled checked' : '') + '>'
                + '<span class="tag">' + (pi === 0 ? '保留' : '副本') + '</span>'
                + '<span class="p" title="' + dkEsc(h.txt) + '\n完整路径：' + dkEsc(p)
                +   '\n点击在资源管理器中定位">' + dkEsc(p) + '</span>'
                + '<button class="sp-act" data-dup-open="' + dkEsc(p) + '" title="在资源管理器中显示所在位置">'
                +   ico('search', 14) + '</button></label>';
            }).join('')
          + '</div>';
      }).join('');
}

/* ── 动作 ───────────────────────────────────────────────────────────────── */
function dkStartScan(drive){
  if(DK.scanning) return;
  DK.drive = drive; DK.scanning = true; DK.ready = false; DK.pct = 0; DK.err = '';
  DK.frn = 5; DK.rows = []; DK.crumbs = []; DK.dirLoaded = false;
  DK.topRows = []; DK.dupGroups = []; DK.dupFreed = 0; DK.stats = {};
  dkPaint();
  callGo('SpaceScanAsync', drive).catch(function(e){
    DK.scanning = false; DK.err = String(e && e.message ? e.message : e);
    dkPaint();
  });
}

function dkSwitchTab(tab){
  DK.tab = tab;
  dkEach('#spSeg .sp-tab', function(b){
    b.classList.toggle('on', b.getAttribute('data-dtab') === tab);
  });
  dkRenderBody();
}

function dkOpenPath(frnOrPath){
  var p = frnOrPath;
  var run = typeof p === 'string'
    ? Promise.resolve(p)
    : callGo('SpacePath', p).catch(function(){ return ''; });
  run.then(function(path){
    if(!path){ toast('没取到这个文件的路径', 'warn'); return; }
    return callGo('SpaceOpen', path);
  }).catch(function(){ toast('打开失败', 'warn'); });
}

/* 删除确认：老版本是弹窗，本版沿用（删文件是要用户拍板的事，不能默默删） */
function dkConfirmDelete(paths, title){
  var box = dkEl('modalBox');
  if(!box){ return; }
  box.style.width = 'min(520px,94vw)';
  box.innerHTML = '<div class="modal-t"><span style="color:var(--danger)">' + ico('warn', 20) + '</span>确认删除</div>'
    + '<div class="modal-d">' + dkEsc(title) + '</div>'
    + '<div class="sp-del-note">文件将移入<b>回收站</b>，误删可还原；确认后立即执行。</div>'
    + '<div class="modal-f"><button class="btn" id="mCancel">取消</button>'
    + '<button class="btn primary" id="spDelGo">' + ico('trash', 16) + '删除</button></div>';
  dkEl('modal').classList.add('open');
  dkEl('mCancel').onclick = closeModal;
  var go = dkEl('spDelGo');
  go.onclick = function(){
    go.disabled = true; go.textContent = '删除中…';
    callGo('SpaceDelete', paths).then(function(res){
      var fail = (res || []).filter(function(r){ return !r.ok; });
      closeModal();
      if(fail.length) toast((paths.length - fail.length) + ' 项已入回收站，' + fail.length + ' 项失败', 'warn');
      else toast(paths.length + ' 项已移入回收站', 'ok');
      if(DK.tab === 'dir'){ DK.rows = []; DK.dirLoaded = false; dkRenderDir(); }
      if(DK.tab === 'top') dkLoadTop();
      if(DK.tab === 'dup'){ DK.dupGroups = []; DK.dupFreed = 0; dkRenderDup(); }
    }).catch(function(e){
      closeModal();
      toast('删除失败：' + (e && e.message ? e.message : e), 'danger');
    });
  };
}

/* ── 页面入口 ───────────────────────────────────────────────────────────── */
window.loadDiskDrives = function(){
  if(!hasGo()) return;
  callGo('GetDiskList').then(function(list){
    DK.drives = list || [];
    if(!DK.drive){
      var ntfs = DK.drives.filter(function(d){ return d.ntfs; });
      DK.drive = (ntfs.sort(function(a, b){ return (b.total - b.free) - (a.total - a.free); })[0]
        || DK.drives[0] || {}).drive || '';
    }
    dkPaint();
  }).catch(function(){});
};

window.bindDisk = function(){
  var root = dkEl('content');
  if(!root) return;

  /* 后端事件只注册一次 */
  if(!DK.evtBound){
    DK.evtBound = true;
    onGo('space:progress', function(pct){
      DK.pct = pct || 0; DK.scanning = true;
      var el = dkEl('spPctText'), bar = dkEl('spPctBar');
      if(el) el.textContent = DK.pct + '%';
      if(bar) bar.style.width = DK.pct + '%';
      var ring = document.querySelector('.sp-scan-ring');
      if(ring) ring.style.setProperty('--p', DK.pct);
    });
    onGo('space:done', function(d){
      DK.scanning = false;
      if(!d || !d.ok){
        DK.ready = false; DK.err = (d && d.err) || '扫描失败';
        dkPaint();
        toast(DK.err, 'danger');
        return;
      }
      DK.ready = true; DK.err = ''; DK.frn = 5; DK.tab = 'dir';
      DK.rows = []; DK.crumbs = []; DK.dirLoaded = false; DK.stats = d;
      dkPaint();
      toast(d.drive + ' 盘索引完成：' + (d.files || 0).toLocaleString() + ' 个文件', 'ok');
    });
    onGo('dup:progress', function(d){
      DK.dupScanning = true; DK.dupPct = (d && d.pct) || 0;
      DK.dupStage = (d && d.stage === 'hashing') ? '哈希比对中' : '分组中';
      var el = dkEl('spDupProg');
      if(el) el.innerHTML = dkDupProgHtml();
      else if(DK.tab === 'dup') dkRenderDup();
    });
    onGo('dup:done', function(d){
      DK.dupScanning = false;
      if(d && d.ok){ DK.dupGroups = d.groups || []; DK.dupFreed = d.freed || 0; }
      else toast((d && d.err) || '重复文件扫描失败', 'warn');
      if(DK.tab === 'dup') dkRenderDup();
    });
  }

  if(root.dataset.diskBound === '1'){ dkRefresh(); return; }
  root.dataset.diskBound = '1';

  root.addEventListener('click', function(e){
    var t = e.target;
    if(!t || !t.closest) return;

    /* 重复文件：定位 / 整组删除（label 里的路径文字也可点，别让它去勾复选框） */
    var dop = t.closest('[data-dup-open]');
    if(dop){ e.preventDefault(); dkOpenPath(dop.getAttribute('data-dup-open')); return; }
    var dp = t.closest('.sp-duppath');
    if(dp && t.classList && t.classList.contains('p')){
      e.preventDefault();
      dkOpenPath(dp.querySelector('[data-dup-open]').getAttribute('data-dup-open'));
      return;
    }
    var ddel = t.closest('.sp-dup-del');
    if(ddel){
      var gi = +ddel.getAttribute('data-gi');
      var g = DK.dupGroups[gi];
      if(!g) return;
      var picked = [];
      dkEach('.sp-dupgroup[data-gi="' + gi + '"] input[data-dup-path]', function(c){
        if(c.checked) picked.push(c.getAttribute('data-dup-path'));
      });
      picked = picked.filter(function(p){ return g.paths.indexOf(p) >= 0 && p !== g.paths[0]; });
      if(!picked.length){ toast('请先勾选要删除的副本（第一份是保留项）', 'warn'); return; }
      dkConfirmDelete(picked, picked.length + ' 个重复副本（共 ' + fmtSize(g.size * picked.length)
        + '），保留：' + g.paths[0]);
      return;
    }

    /* 列表里的两个小按钮：定位 / 删除 */
    var op = t.closest('[data-open-path]');
    if(op){ dkOpenPath(op.getAttribute('data-open-path')); return; }
    var dp2 = t.closest('[data-del-path]');
    if(dp2){ dkConfirmDelete([dp2.getAttribute('data-del-path')], dp2.getAttribute('data-del-path')); return; }
    var o1 = t.closest('[data-open]');
    if(o1){ e.stopPropagation(); dkOpenPath(+o1.getAttribute('data-open')); return; }
    var d1 = t.closest('[data-del]');
    if(d1){
      e.stopPropagation();
      callGo('SpacePath', +d1.getAttribute('data-del')).then(function(p){
        if(p) dkConfirmDelete([p], p);
      }).catch(function(){ toast('没取到路径', 'warn'); });
      return;
    }

    /* 目录分析：钻取（面包屑与目录名共用 data-frn） */
    var fr = t.closest('[data-frn]');
    if(fr){ dkDrill(+fr.getAttribute('data-frn')); return; }

    /* 视图切换 / 排序 / 显示更多 */
    var vw = t.closest('[data-view]');
    if(vw){ DK.view = vw.getAttribute('data-view'); dkRenderDir(); return; }
    var st = t.closest('[data-sort]');
    if(st){
      var k = st.getAttribute('data-sort');
      if(DK.sortBy === k) DK.sortDir = -DK.sortDir;
      else { DK.sortBy = k; DK.sortDir = (k === 'name') ? 1 : -1; }
      dkRenderDir();
      return;
    }
    if(t.closest('#spMore')){ DK.showN += 500; dkRenderDir(); return; }

    /* 矩形图：点方块钻进去 */
    if(t.id === 'spMap'){
      var hit = dkMapHit(t, e);
      if(hit) dkDrill(hit.r.frn);
      return;
    }

    /* Tab / 磁盘卡 / 各页按钮 */
    var tb = t.closest('[data-dtab]');
    if(tb){ dkSwitchTab(tb.getAttribute('data-dtab')); return; }
    var dv = t.closest('[data-drive]');
    if(dv){
      var dr = dv.getAttribute('data-drive');
      if(dr !== DK.drive) dkStartScan(dr);
      return;
    }
    if(t.closest('#spScan')){ dkStartScan(DK.drive); return; }
    if(t.closest('#spTopGo')){ dkLoadTop(); return; }
    if(t.closest('#spDupGo')){
      var sel = dkEl('spDupMin');
      if(sel) DK.dupMin = +sel.value;
      DK.dupScanning = true; DK.dupGroups = []; DK.dupFreed = 0;
      dkRenderDup();
      callGo('SpaceDupScanAsync', DK.dupMin).catch(function(ex){
        DK.dupScanning = false;
        toast('查找失败：' + (ex && ex.message ? ex.message : ex), 'warn');
        dkRenderDup();
      });
      return;
    }
  });

  /* 阈值下拉：改了就重新取（change 事件，不是 click） */
  root.addEventListener('change', function(e){
    if(e.target && e.target.id === 'spTopMin') dkLoadTop();
  });

  dkRefresh();
};

/* 进页：盘列表只拉一次（它不变），已经在手上就直接画 */
function dkRefresh(){
  if(!DK.drives.length) window.loadDiskDrives();
  else dkPaint();
}
