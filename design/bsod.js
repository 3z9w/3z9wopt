/* ══════════════════════════════════════════════════════════════════════════
   蓝屏分析 · 独立模块（design/bsod.js）
   ---------------------------------------------------------------------------
   功能口径照老版本（v2 蓝屏分析）：扫本机转储 → 解析 BugCheck 代码与 4 个参数 →
   给中文结论、常见原因、处理建议与嫌疑驱动（纯 Go 读转储头部，不需要 WinDbg /
   符号服务器 / PowerShell）。本版新增「打开蓝屏文件」——可以挑任意一个 .dmp
   来分析（朋友发来的、旧硬盘拷出来的），不只局限本机 Minidump 目录。
   后端接口：BsodList / BsodDetail / BsodDumpDir / BsodPickFile / BsodReveal。
   视觉按 1.1：玻璃卡 + 蓝为主（配色基准 = 本机配置页），琥珀只留给风险提示条。
   ⚠️ 状态与函数一律挂 window（加载器是 IIFE 包着的 eval，见 ui-prototype.html 的
      「蓝屏分析页独立文件加载器」），否则 PAGES 引用不到 → 整页白屏。
   ══════════════════════════════════════════════════════════════════════════ */

/* seq：弹窗代次。换一个蓝屏文件就 +1 —— 上一次的异步解析回来时如果代次对不上，
   结果直接丢弃（不然先开的大文件会晚一步回来、把后开的小文件结果盖掉）。 */
var BS_S = {loaded:false, loading:false, err:'', list:[], dir:'', busy:false, seq:0};

function bsEsc(s){ return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
  return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];
});}
function bsEl(id){ return document.getElementById(id); }

/* 转储目录：后端给真路径（系统盘不一定是 C:）；还没取到就别编，写「未获取到」 */
function bsDirTxt(){ return BS_S.dir || '未获取到'; }

/* ── 页面骨架 ───────────────────────────────────────────────────────────── */
function renderBsod(){
  return '<div class="page-head"><div class="page-title">蓝屏分析</div>'
    + '<div class="page-sub">读出 Windows 蓝屏时存的<b>转储记录</b>，看是哪次、什么错误码、'
    + '大概是谁引起的；读不出结论的会写明原因，不硬编。</div></div>'

    /* hero：一眼看到「几条记录 / 最近一次 / 最常出现」 */
    + '<div class="bs-hero">'
    +   '<div class="bs-hero-ico">' + ico('warn', 21) + '</div>'
    +   '<div class="bs-hero-main">'
    +     '<div class="bs-hero-t">本机蓝屏转储，按发生时间倒序</div>'
    +     '<div class="bs-hero-d">转储目录 <b id="bsDirTxt">' + bsEsc(bsDirTxt()) + '</b>（含 MEMORY.DMP）；'
    +     '点任一条看参数与嫌疑驱动，也可以<b>打开蓝屏文件</b>看别的机器拷来的转储。</div>'
    +   '</div>'
    +   '<div class="bs-kpis">'
    +     '<div class="bs-kpi"><span>转储记录</span><b id="bsKAll">—</b></div>'
    +     '<div class="bs-kpi sm"><span>最近一次</span><b id="bsKLast">—</b></div>'
    +     '<div class="bs-kpi sm" id="bsKTopBox"><span>最常出现</span><b id="bsKTop">—</b></div>'
    +   '</div>'
    + '</div>'

    /* 提示条：只读不改 + 完整释义在线查 */
    + '<div class="bs-tip">' + ico('info', 15)
    +   '<span>本页只<b>读取</b>转储文件，不改动、不删除任何东西；'
    +   '这里给排查方向，完整释义见 <a data-bs-site title="打开 3z9w.com/bsod">3z9w.com/bsod</a>。</span></div>'

    /* 列表区（自己滚）；条目：[错误码] 标题+中文 / 时间·系统·大小 / 分析 */
    + '<div class="bs-list" id="bsList">' + bsListHtml() + '</div>'

    /* 底部操作栏：常驻可见（页面不滚，只有列表滚） */
    + '<div class="bs-foot">'
    +   '<button class="bs-btn" data-bs-open id="bsOpen">' + ico('folder', 14) + '打开蓝屏文件</button>'
    +   '<span class="bs-count" id="bsCount">—</span>'
    +   '<span class="sp"></span>'
    +   '<button class="bs-btn" data-bs-site>' + ico('globe', 14) + '在线查完整释义</button>'
    +   '<button class="bs-btn pri" data-bs-reload id="bsReload">' + ico('refresh', 14) + '重新扫描</button>'
    + '</div>';
}

/* ── 数据 ───────────────────────────────────────────────────────────────── */
function bsodLoad(){
  if(BS_S.loading) return;
  BS_S.loading = true;
  bsSetBusy(true);
  callGo('BsodList').then(function(l){
    BS_S.list = l || [];
    BS_S.loaded = true; BS_S.loading = false; BS_S.err = '';
    bsSetBusy(false);
    bsRender();
    return callGo('BsodDumpDir').then(function(d){ BS_S.dir = d || ''; bsPaintDir(); }).catch(function(){});
  }).catch(function(e){
    BS_S.loading = false; BS_S.err = String(e);
    bsSetBusy(false);
    bsRender();
  });
}

/* ── 局部重绘（不整页 render，保住滚动位置与按钮可用性）─────────────────── */
function bsRender(){
  var host = bsEl('bsList');
  if(host) host.innerHTML = bsListHtml();
  bsPaintStats();
  bsPaintDir();
}

function bsPaintDir(){
  var el = bsEl('bsDirTxt');
  if(el) el.textContent = bsDirTxt();
}

function bsPaintStats(){
  var list = BS_S.list, valid = bsValid();
  var ok = BS_S.loaded && !BS_S.err;
  var a = bsEl('bsKAll');
  if(a) a.textContent = ok ? String(list.length) : '—';
  var last = bsEl('bsKLast');
  if(last) last.textContent = !ok ? '—' : (valid.length ? (valid[0].time || '—') : '无记录');
  var top = bsTopCode();
  var tb = bsEl('bsKTop');
  if(tb) tb.textContent = !ok ? '—' : (top || '无记录');
  var box = bsEl('bsKTopBox');
  if(box) box.className = 'bs-kpi sm' + (top ? ' on' : '');
  var c = bsEl('bsCount');
  if(c){
    c.innerHTML = !ok ? '—'
      : (list.length ? ('共 <b>' + list.length + '</b> 条转储 · 解析成功 <b>' + valid.length + '</b> 条') : '暂无转储记录');
  }
}

function bsValid(){
  var out = [];
  for(var i = 0; i < BS_S.list.length; i++) if(BS_S.list[i].valid) out.push(BS_S.list[i]);
  return out;
}
/* 出现最多的错误符号名（并列的取第一条，和列表顺序一致） */
function bsTopCode(){
  var m = {}, order = [];
  var v = bsValid();
  for(var i = 0; i < v.length; i++){
    var k = v[i].title || v[i].codeHex || '未知';
    if(!(k in m)){ m[k] = 0; order.push(k); }
    m[k]++;
  }
  var best = '', n = 0;
  for(var j = 0; j < order.length; j++){
    if(m[order[j]] > n){ n = m[order[j]]; best = order[j]; }
  }
  return n > 1 ? (best + '（' + n + ' 次）') : best;
}

/* ── 列表 ───────────────────────────────────────────────────────────────── */
function bsListHtml(){
  if(!BS_S.loaded && !BS_S.err){
    return '<div class="bs-empty"><div class="bs-empty-ico">' + ico('refresh', 26) + '</div>'
      + '<div class="bs-empty-t">正在扫描转储文件…</div></div>';
  }
  if(BS_S.err){
    return '<div class="bs-empty"><div class="bs-empty-ico err">' + ico('warn', 26) + '</div>'
      + '<div class="bs-empty-t">没能扫到转储文件</div>'
      + '<div class="bs-empty-d">' + bsEsc(BS_S.err) + '</div>'
      + '<div class="bs-empty-acts"><button class="bs-btn pri" data-bs-reload>' + ico('refresh', 14) + '重新扫描</button></div></div>';
  }
  if(!BS_S.list.length){
    return '<div class="bs-empty"><div class="bs-empty-ico ok">' + ico('check', 26) + '</div>'
      + '<div class="bs-empty-t">没有蓝屏转储</div>'
      + '<div class="bs-empty-d">这台机器在 <b>' + bsEsc(bsDirTxt()) + '</b> 里没有转储文件 —— 好事。'
      + '如果确实蓝屏过却看不到记录，检查：系统属性 → 高级 → 启动和故障恢复 →「写入调试信息」应选「小内存转储」。</div>'
      + '<div class="bs-empty-acts"><button class="bs-btn pri" data-bs-open>' + ico('folder', 14) + '打开蓝屏文件</button></div></div>';
  }
  return BS_S.list.map(function(d, i){
    var bad = !d.valid;
    return '<div class="bs-item' + (bad ? ' bad' : '') + '" data-bs-i="' + i + '">'
      + '<div class="bs-code">' + bsEsc(d.codeHex || '—') + '</div>'
      + '<div class="bs-mid">'
      +   '<div class="bs-t">' + bsEsc(d.title || (bad ? '解析不了这条记录' : '未知错误'))
      +     (d.zh ? '<span class="bs-zh">' + bsEsc(d.zh) + '</span>' : '') + '</div>'
      +   '<div class="bs-d">' + bsEsc(d.time || d.name || '') + ' · ' + bsEsc(bad ? (d.err || '转储头部不完整') : bsMeta(d)) + '</div>'
      + '</div>'
      + '<button class="bs-go" data-bs-i="' + i + '">' + (bad ? '详情' : (ico('search', 13) + '分析')) + '</button>'
      + '</div>';
  }).join('');
}
function bsMeta(d){
  var s = [];
  if(d.os) s.push(d.os);
  if(d.arch) s.push(d.arch);
  s.push(bsSize(d.size || 0));
  return s.join(' · ');
}
function bsSize(b){
  return (typeof fmtSize === 'function') ? fmtSize(b)
    : (b > 1048576 ? (b / 1048576).toFixed(1) + ' MB' : Math.max(1, Math.round(b / 1024)) + ' KB');
}

/* ── 详情弹窗 ───────────────────────────────────────────────────────────── */
/* 传 index 从列表取；传路径（字符串）走「打开蓝屏文件」，列表里可能没有 */
/* 弹窗副标题：有效记录给中文结论，解析不了的给失败原因（别留空） */
function bsHeadText(d){
  if(!d) return '正在读取转储…';
  return d.valid ? (d.zh || d.cause || '') : (d.err || '这条记录解析不了');
}
function bsodOpen(i){
  var d = BS_S.list[i] || null;
  bsShowDetail(d, d ? d.file : String(i), false);
}

/* deep=true 表示 d 已经是 BsodDetail 深析过的（「打开蓝屏文件」那条路），
   不用再解析一遍 —— 同一个文件解析两次，慢一倍还没意义。 */
function bsShowDetail(d, file, deep){
  var box = bsEl('modalBox'), mod = bsEl('modal');
  if(!box || !mod || typeof closeModal !== 'function'){ return; }
  BS_S.seq++;
  var title = (d && d.valid) ? ((d.title || '蓝屏记录') + (d.codeHex ? ' · ' + d.codeHex : '')) : '蓝屏转储';
  var sub = !d ? '正在读取转储…' : bsHeadText(d);
  box.innerHTML = '<div class="modal-t">' + ico('warn', 18) + '<span>' + bsEsc(title) + '</span></div>'
    + '<div class="modal-d">' + bsEsc(sub) + '</div>'
    + '<div class="bs-detail" id="bsDetailBox">'
    +   bsDetailInner(d, true)
    + '</div>'
    + '<div class="modal-f">'
    +   '<button class="btn" id="bsDReveal">' + ico('folder', 14) + '在文件夹里显示</button>'
    +   '<button class="btn" id="bsDSite">' + ico('globe', 14) + '在线查完整释义</button>'
    +   '<button class="btn" id="bsDDriver">' + ico('cpu', 14) + '去驱动管理</button>'
    +   '<button class="btn" id="bsDSvc">' + ico('message', 14) + '人工远程处理</button>'
    +   '<button class="btn primary" id="bsDClose">关闭</button>'
    + '</div>';
  /* 驱动表列多（10 列），这个弹窗按内容加宽 —— closeModal() 会还原，不影响别的弹窗 */
  box.style.width = 'min(900px,94vw)';
  mod.classList.add('open');
  var c = bsEl('bsDClose'); if(c) c.onclick = closeModal;
  var rv = bsEl('bsDReveal');
  if(rv) rv.onclick = function(){
    callGo('BsodReveal', file || (d ? d.file : '')).then(function(err){
      if(err) toast(err, 'warn');
    }).catch(function(e){ toast('打开失败：' + e, 'warn'); });
  };
  var st = bsEl('bsDSite');
  if(st) st.onclick = function(){ bsSite(d ? d.codeHex : ''); };
  var dv = bsEl('bsDDriver');
  if(dv) dv.onclick = function(){ closeModal(); if(typeof go === 'function') go('driver'); };
  var sc = bsEl('bsDSvc');
  if(sc) sc.onclick = function(){
    closeModal();
    if(typeof go === 'function') go('svc');
    /* 服务大厅是按需读取的，第一次点进来可能还没数据 —— 补一次（幂等，读到会自己重绘） */
    if(typeof loadSvc === 'function') loadSvc();
  };
  if(deep){ return; }              /* 已经是深析结果，直接显示 */
  bsFillDetail(d, file);
}

/* 弹窗内容（深析前后共用；pending=true 时栈和嫌疑驱动显示「解析中…」）
   两层排版：先「人话」（小白看得懂在说什么、该做什么），再「原始字段」（工程师要的
   错误码 / 参数 / 调用栈 / 驱动表）。专业信息默认展开 —— 会点进这个页面的人多半就要看这些。 */
function bsDetailInner(d, pending){
  if(!d){
    return '<div class="bs-none">正在读取转储…</div>';
  }
  if(!d.valid){
    return '<div class="bs-sec">读不出来</div><div class="bs-param">' + bsEsc(d.err || '不是有效的内核转储')
      + '</div><div class="bs-note">这条记录只列出来留个底，看不出错误码 —— 换一份完整的 .dmp 再来。</div>';
  }
  var h = '';
  /* 一层：人话 */
  h += '<div class="bs-verdict"><div class="bs-verdict-t">' + ico('info', 15)
    + '<span>这是什么情况</span></div>' + bsPlainHtml(d) + '</div>';
  /* 二层：该怎么做 */
  if(d.advice && d.advice.length){
    h += '<div class="bs-sec">该怎么做（按顺序来）</div>' + d.advice.map(function(a, i){
      return '<div class="bs-adv"><b class="bs-step">' + (i + 1) + '</b><span>' + bsEsc(a) + '</span></div>';
    }).join('');
  }
  /* 三层：原始字段 */
  h += '<details class="bs-pro" open><summary>专业信息 · 错误码 / 参数 / 调用栈 / 驱动表</summary>'
    + '<div class="bs-fields">'
    + bsField('故障检查代码', (d.codeHex || '未获取到') + (d.title ? ('　' + d.title) : ''), true)
    + bsField('崩溃镜像', d.image || (pending ? '解析中…' : '未获取到'))
    + bsField('崩溃点', (d.crashIn || '未归属到驱动') + (d.crashAt ? ('　' + d.crashAt) : ''), true)
    + bsField('模块+偏移', d.symbol || (pending ? '解析中…' : '未获取到'), true)
    + bsField('故障桶', d.bucket || (pending ? '解析中…' : '未获取到'), true)
    + bsField('运行时长', d.uptime || '未获取到')
    + bsField('发生时间', d.time || '未获取到')
    + bsField('系统', ((d.os || '未获取到') + (d.build ? (' · 内部版本 ' + d.build) : '') + (d.arch ? (' · ' + d.arch) : '')))
    + bsField('转储文件', d.file || '未获取到')
    + '</div>';
  if(d.params && d.params.length){
    h += '<div class="bs-sec">错误参数</div>' + d.params.map(function(p){ return '<div class="bs-param">' + bsEsc(p) + '</div>'; }).join('');
  }
  h += '<div class="bs-sec">调用栈</div><div id="bsStackBox">'
    + (pending ? '<div class="bs-none">解析中…</div>' : bsStackHtml(d)) + '</div>';
  h += '<div class="bs-sec">嫌疑驱动</div><div id="bsDrvBox">'
    + (pending ? '<div class="bs-none">解析中…</div>' : bsDrvHtml(d)) + '</div>';
  if(d.modules && d.modules.length) h += bsModulesHtml(d.modules);
  h += '</details>';
  if(d.note) h += '<div class="bs-note">' + bsEsc(d.note) + '</div>';
  return h;
}

function bsField(k, v, mono){
  return '<div class="bs-kv"><span>' + bsEsc(k) + '</span><b'
    + (mono ? ' class="mono"' : '') + '>' + bsEsc(v || '未获取到') + '</b></div>';
}

/* 人话：把「错误码 + 被点名的驱动 + 常见原因」串成几句普通人能读懂的话 */
function bsPlainHtml(d){
  var out = [];
  out.push('Windows 遇到了让它没法继续跑下去的严重错误，为了保护硬件和数据不再受损，它主动停下电脑并显示蓝屏。');
  var l2 = '这次的错误是 <b>' + bsEsc(d.title || d.codeHex || '未知') + '</b>：'
    + bsEsc(d.zh || '内置码表没收录这个代码') + '。';
  var who = d.image || '';
  if(who){
    var comp = '';
    if(d.drivers){
      for(var i = 0; i < d.drivers.length; i++){
        if(d.drivers[i].name === who){
          var t = [];
          if(d.drivers[i].comp) t.push(d.drivers[i].comp);
          if(d.drivers[i].desc) t.push(d.drivers[i].desc);
          comp = t.join(' · ');
          break;
        }
      }
    }
    l2 += ' 被系统点名的是 <b class="mono">' + bsEsc(who) + '</b>'
      + (comp ? ('（' + bsEsc(comp) + '）') : '') + '。';
  }
  out.push(l2);
  var bits = [];
  if(d.uptime) bits.push('开机后跑了 <b>' + bsEsc(d.uptime) + '</b> 才出的事');
  if(d.time) bits.push('发生时间 <b>' + bsEsc(d.time) + '</b>');
  if(bits.length) out.push(bits.join('，') + '。');
  if(d.cause) out.push('通俗说：' + bsEsc(d.cause) + '。');
  return out.map(function(x){ return '<p class="bs-plain">' + x + '</p>'; }).join('');
}

/* 该转储装载的内核驱动（BlueScreenView 下方那张表）：默认收起，点开才铺。
   栈内命中的行高亮 —— 那就是「谁把系统搞崩的」。 */
function bsModulesHtml(mods){
  var stack = 0;
  for(var i = 0; i < mods.length; i++) if(mods[i].inStack) stack++;
  var rows = mods.map(function(m){
    return '<tr' + (m.inStack ? ' class="hit"' : '') + '>'
      + '<td class="nm">' + bsEsc(m.name || '—') + (m.inStack ? '<i>栈内</i>' : '') + '</td>'
      + '<td class="mono">' + bsEsc(m.off || m.hit || '—') + '</td>'
      + '<td class="mono">' + bsEsc(m.base || '') + '</td>'
      + '<td class="mono">' + bsEsc(m.end || '') + '</td>'
      + '<td class="mono">' + bsEsc(m.size || '') + '</td>'
      + '<td>' + bsEsc(m.stamp || '—') + '</td>'
      + '<td>' + bsEsc(m.comp || '—') + (m.verLocal ? BS_OTH : '') + '</td>'
      + '<td>' + bsEsc(m.desc || '—') + (m.verLocal ? BS_OTH : '') + '</td>'
      + '<td>' + bsEsc(m.ver || '—') + (m.verLocal ? BS_OTH : '') + '</td>'
      + '<td class="pth" title="' + bsEsc(m.path || '') + '">' + bsEsc(m.path || '—') + '</td>'
      + '</tr>';
  }).join('');
  return '<details class="bs-mods"><summary>该转储装载的内核驱动 · <b>' + mods.length + '</b> 个'
    + (stack ? ('（栈内命中 <b>' + stack + '</b> 个已高亮）') : '') + '</summary>'
    + '<div class="bs-tbl-wrap"><table class="bs-tbl">'
    + '<thead><tr><th>文件名</th><th>命中</th><th>起始地址</th><th>结束地址</th><th>大小</th>'
    + '<th>时间戳</th><th>公司</th><th>文件描述</th><th>文件版本</th><th>完整路径</th></tr></thead>'
    + '<tbody>' + rows + '</tbody></table></div>' + bsOthNote(mods) + '</details>';
}

/* ── 别的机器的转储：版本几列是从本机同名文件读来的，必须标出来 ──────────────
   转储里只有模块名和地址，公司 / 描述 / 版本这些得去磁盘上找文件读 —— 看别人机器
   （或旧系统）的转储时，本机那个同名文件多半不是同一个版本。用转储里镜像自带的
   PE 时间戳对一下，对不上就打 ≈ 并说明，绝不拿本机的冒充出事那台机器的。 */
var BS_OTH = '<i class="bs-oth" title="本机同名文件的版本，与转储里的镜像对不上 —— 不是出事那台机器的版本">≈</i>';
function bsOthNote(arr){
  var n = 0;
  for(var i = 0; i < arr.length; i++) if(arr[i].verLocal) n++;
  if(!n) return '';
  return '<div class="bs-note">带 <b>≈</b> 的 ' + n + ' 行：公司 / 描述 / 版本是从<b>这台机</b>的同名文件读来的，'
    + '和转储里那份镜像对不上（多半在看别的机器或旧系统的转储）—— 那几列仅供参考，别拿它当出事那台机器的版本。</div>';
}
/* 调用栈：只列「上一条指令确实是 call」的返回地址，没还原出来就直说，不编帧 */
function bsStackHtml(d){
  if(!(d.stack && d.stack.length)){
    return '<div class="bs-none">这份转储没还原出调用栈'
      + (d.note ? (' —— ' + bsEsc(d.note)) : '') + '</div>';
  }
  var rows = d.stack.map(function(f){
    return '<tr><td class="mono">' + (f.no == null ? '—' : f.no) + '</td>'
      + '<td class="mono">' + bsEsc(f.addr || '—') + '</td>'
      + '<td class="nm">' + bsEsc(f.module || '—') + '</td>'
      + '<td class="mono">' + bsEsc(f.off || '—') + '</td>'
      + '<td>' + bsEsc(f.call || '—') + '</td></tr>';
  }).join('');
  return '<div class="bs-tbl-wrap"><table class="bs-tbl">'
    + '<thead><tr><th>#</th><th>返回地址</th><th>模块</th><th>模块+偏移</th><th>校验</th></tr></thead>'
    + '<tbody>' + rows + '</tbody></table></div>'
    + '<div class="bs-note">0 层是最内层（最后出事的那层），往下是它的调用者 —— 和 WinDbg 的 kb 一个顺序。'
    + '没有符号，函数名拿不到，只到「模块+偏移」。<b>校验</b>列写的是这一层怎么确认的：'
    + '「直接调用 / 间接调用」= 往前一条指令确实是 call（可信）；'
    + '「未校验」= 这份转储没存指令页，只能按栈内出现顺序列出，里面可能混着参数或数据地址。</div>';
}

function bsDrvHtml(d){
  if(!(d.drivers && d.drivers.length)){
    return '<div class="bs-none">未能定位栈内驱动' + (d.note ? (' —— ' + bsEsc(d.note)) : '') + '</div>';
  }
  var rows = d.drivers.map(function(x){
    return '<tr>'
      + '<td class="nm">' + bsEsc(x.name || '—') + '</td>'
      + '<td class="mono">' + bsEsc(x.off || '—') + '</td>'
      + '<td class="mono">' + bsEsc(x.base || '—') + '</td>'
      + '<td class="mono">' + bsEsc(x.size || '—') + '</td>'
      + '<td>' + bsEsc(x.time || '—') + '</td>'
      + '<td>' + bsEsc(x.comp || '—') + (x.verLocal ? BS_OTH : '') + '</td>'
      + '<td>' + bsEsc(x.desc || '—') + (x.verLocal ? BS_OTH : '') + '</td>'
      + '<td>' + bsEsc(x.ver || '—') + (x.verLocal ? BS_OTH : '') + '</td>'
      + '</tr>';
  }).join('');
  return '<div class="bs-tbl-wrap"><table class="bs-tbl">'
    + '<thead><tr><th>文件名</th><th>命中位置</th><th>基址</th><th>大小</th>'
    + '<th>时间戳</th><th>公司</th><th>文件描述</th><th>文件版本</th></tr></thead>'
    + '<tbody>' + rows + '</tbody></table></div>' + bsOthNote(d.drivers);
}

/* 深度分析（嫌疑驱动归属）完成后只换内容区，弹窗不闪 */
function bsFillDetail(d, file){
  if(!file) return;
  var seq = BS_S.seq;
  callGo('BsodDetail', file).then(function(full){
    /* 期间又换了文件 → 这份结果作废，别盖掉新弹窗 */
    if(seq !== BS_S.seq) return;
    var box = bsEl('bsDetailBox');
    if(!box || !full) return;
    box.innerHTML = bsDetailInner(full, false);
    /* 标题栏跟着真数据走（打开外部文件时列表里没有，标题得等解析完才有） */
    var mt = document.querySelector('#modalBox .modal-t span');
    if(mt) mt.textContent = (full.title || '蓝屏记录') + (full.codeHex ? (' · ' + full.codeHex) : '');
    var md = document.querySelector('#modalBox .modal-d');
    if(md) md.textContent = bsHeadText(full);
  }).catch(function(e){
    if(seq !== BS_S.seq) return;
    /* 深析只负责「嫌疑驱动归属」这一层：取不回来就保留头部结论，别把整条记录说成失败 */
    if(!d){
      var box = bsEl('bsDetailBox');
      if(box) box.innerHTML = '<div class="bs-none">解析失败：' + bsEsc(String(e)) + '</div>';
    }else{
      toast('深度分析没取回结果（' + e + '），上面是转储头部的结论', 'warn');
    }
  });
}

/* ── 打开蓝屏文件：挑一个 .dmp 直接分析（不只局限本机转储目录）───────────── */
function bsPick(){
  if(BS_S.busy) return;
  callGo('BsodPickFile').then(function(p){
    if(!p) return;                       /* 取消选中，静默返回 */
    bsBusy(true);
    toast('正在解析 ' + p, 'info');
    return callGo('BsodDetail', p).then(function(d){
      bsBusy(false);
      if(!d){ toast('没读到这个转储文件', 'warn'); return; }
      bsShowDetail(d, p, true);
      if(!d.valid) toast('这个文件解析不出蓝屏信息：' + (d.err || ''), 'warn');
    });
  }).catch(function(e){
    bsBusy(false);
    toast('打开失败：' + e, 'danger');
  });
}
function bsBusy(on){
  BS_S.busy = !!on;
  bsSetBusy(on);
}
function bsSetBusy(on){
  var b = bsEl('bsReload'), o = bsEl('bsOpen');
  if(b) b.disabled = !!on;
  if(o) o.disabled = !!on;
}

function bsSite(code){
  var u = 'https://www.3z9w.com/bsod';
  if(code) u += '?code=' + encodeURIComponent(code);
  if(typeof window.tbOpen === 'function'){ window.tbOpen(u); return; }
  try{ window.runtime.BrowserOpenURL(u); }
  catch(_){ toast('浏览器打开失败：' + u, 'warn'); }
}

/* ── 事件：整页委托挂 #content（只换 innerHTML，元素永不重建）──────────── */
function bsClick(e){
  var t = e.target;
  while(t && t !== e.currentTarget){
    if(t.tagName === 'A'){
      if(t.hasAttribute && t.hasAttribute('data-bs-site')){ e.preventDefault(); bsSite(''); }
      return;
    }
    if(t.hasAttribute){
      if(t.hasAttribute('data-bs-i')){
        var i = parseInt(t.getAttribute('data-bs-i'), 10);
        bsodOpen(isNaN(i) ? 0 : i);
        return;
      }
      if(t.hasAttribute('data-bs-open')){ bsPick(); return; }
      if(t.hasAttribute('data-bs-reload')){ toast('正在扫描转储…', 'info'); bsodLoad(); return; }
      if(t.hasAttribute('data-bs-site')){ bsSite(''); return; }
    }
    t = t.parentNode;
  }
}

/* ── 绑定与进页补画 ─────────────────────────────────────────────────────── */
function bindBsod(){
  var root = bsEl('content');
  if(!root) return;
  if(root.getAttribute('data-bsBound') !== '1'){
    root.setAttribute('data-bsBound', '1');
    root.addEventListener('click', bsClick);
  }
  bsAfterPaint();
}
function bsAfterPaint(){
  if(!BS_S.loaded && !BS_S.loading){ bsodLoad(); return; }
  /* 数据已在手（启动预热过）也要补画一次：HTML 里 KPI 是先占位写的 '—' */
  bsRender();
}

window.renderBsod   = renderBsod;
window.bindBsod     = bindBsod;
window.bsodLoad     = bsodLoad;
window.BS_S         = BS_S;
