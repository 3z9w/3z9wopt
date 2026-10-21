/* ============================================================================
   pcinfo.js —— 本机配置页（独立文件 · 2026-10-10 第二版 · 全面重做）
   ---------------------------------------------------------------------------
   为什么独立：这一页后面要反复改（布局、Tab、字段），塞在 5000+ 行的大文件里
   改起来互相干扰、不好回滚。所以整页搬出来，主文件只留一个挂载点。

   载入方式：**主文件用同步 XHR 把它读进来 eval**，不是 <script src>。
   原因很硬：Wails 只对 /index.html 注入 runtime.js / ipc.js，异步 script
   会晚于 window.go 就位 → 这一页首帧就读不到后端（表现为「未获取到」一片）。
   同步加载保证 go('pcinfo') 之前定义已全部就位。

   第一版被否（"太一般"）：整页 label —— 值两列平铺，像报表，没图形没颜色没层次。
   这一版四条主张：
     ① 关键数据图形化 —— 占用/容量给环，数字与环同步（数值↔视觉一一对应）
     ② 状态用色点 —— 安全启动开=绿、关=橙；这是真实状态映射，不是装饰
     ③ 品牌色小面积强调 —— 分组标题色条 / 选中 Tab / 关键数值
     ④ 一屏装完 —— 别让用户拉右侧滚动条

   数据口径（全页统一，不许各写各的）：
     · 取不到 → 「未获取到」（数据缺失）
     · 操作出错 → 「读取失败：xxx」（真出错，不许淡化）
     · 温度一律标「温度区」—— 固件不告诉我们那是 CPU 还是主板，猜了就是编
   ============================================================================ */
(function(){
'use strict';

/* ── 状态 ──
   ⚠️ 这三个必须挂在 window 上，不能只做文件内的局部变量：
   主页面（ui-prototype.html）也要读 window.PC —— 首页那半句「已开机」就是从
   window.PC.uptime 拿的。如果它们只在 eval 的局部作用域里，主页面拿到的是另一个
   undefined，两边各看各的、同步不了。 */
window.PC = null; window.pcState = 'loading'; window.pcErr = ''; window.pcTab = 'sum';
const PC_NA = '未获取到';

function pcGb(b){ return Math.round((b || 0) / 1e8) / 10; }

/* ── 加载 ── */
/* 最短忙碌时长：本机读得快时（几百毫秒）动画会一闪而过，反而像「点了没反应」。
   设个下限，转圈和扫光至少能被看见一次。 */
const PC_BUSY_MIN = 460;

/* ⚠️⚠️ 「点了一下没反应」的真因（2026-10-10 实测复现，见 _devtools/pc_click_race.js）
   ---------------------------------------------------------------------------
   现象：点概要清单某一行后面的「>」，第一次没跳转，再点一次就好了。
   不是玄学偶发，是**必然的竞态**：GetPCFull 回来后走了整页 render()，
   #pcBody 整个被换掉；如果用户这一刻正按着鼠标（mousedown 已发生、mouseup 还没来），
   抬起时命中的是新 DOM —— 浏览器只在 mousedown / mouseup 命中同一元素（或其祖先）
   时才派发 click，于是这一次点击被**静默吞掉**，没有任何报错。
   探针实测：按下 → 中间 render() → 抬起，Tab 停在 sum 没动；同样位置不改时序
   再点一次就正常。
   两条措施一起上，缺一不可：
     ① 不再在用户按着鼠标的时候重建内容区（pcSafeRedraw）；
     ② 不在用户已经看着内容的时候整页 render（pcLoadDone 改走局部重绘）；
     ③ 同一份数据不许并发读两次（loadPC 去重），少一次重绘就少一次机会；
     ④ 兜底：点击判定不依赖 DOM 存活（bindPcinfo 里的 pointerdown/pointerup）。
   --------------------------------------------------------------------------- */

/* 指针是否正按着（由 bindPcinfo 在 document 上捕获阶段维护）。
   重绘撞上这一下就会吞 click，所以所有"非用户主动"的重绘都要避开它。 */
function pcSafeRedraw(fn){
  if(!window.__pcPress){ fn(); return; }
  let done = false;
  const run = function(){
    if(done) return; done = true;
    document.removeEventListener('pointerup', run, true);
    /* 抬手后留 80ms：让这一下 click 先派发完，再动 DOM */
    setTimeout(fn, 80);
  };
  document.addEventListener('pointerup', run, true);
  /* 兜底：pointerup 万一收不到（指针移出窗口 / 异常中断），也不能永远不画 */
  setTimeout(run, 700);
}

function loadPC(){
  if(!hasGo()){ if(inWails()) window.pcState = 'failed'; return; }
  /* 同一时刻只飞一个 GetPCFull：自愈重跑、F5、手动刷新可能叠在一起，
     两次返回就重绘两次 —— 每多一次重绘就多一次吞掉用户点击的机会。 */
  if(window.__pcInflight) return;
  window.__pcInflight = true;
  const settle = function(){ window.__pcInflight = false; };
  const busy = !!window.__pcBusy;
  window.pcState = 'loading';
  const t0 = Date.now();
  callGo('GetPCFull').then(function(f){
    window.PC = f || {}; window.pcState = 'ready'; window.pcErr = '';
    settle(); pcLoadDone(t0, busy, '');
  }).catch(function(e){
    window.pcErr = String(e);
    /* 手动刷新失败时**保留上一次的好数据**（页面别变成错误页），
       只在本来就没数据时才进失败态。 */
    window.pcState = window.PC ? 'ready' : 'failed';
    settle(); pcLoadDone(t0, busy, String(e));
  });
}

/* 读完统一收口。
   两条路径分开：
     · 首次进入（busy=false）→ 整页 render（这时候页面本来就是骨架，重画无妨）
     · 手动刷新（busy=true）→ 只重绘横幅与内容区，**面板骨架和 Tab 条不动**，
       旧数据一直在屏幕上淡着，不是白屏 —— 这才是「载入」而不是「重画」 */
function pcLoadDone(t0, busy, err){
  const wait = Math.max(0, PC_BUSY_MIN - (Date.now() - t0));
  setTimeout(function(){
    if(busy){
      pcBusyEnd();
      if(err){ toast('读取失败：' + err, 'danger'); return; }
      pcSafeRedraw(pcRepaint);
      return;
    }
    /* ⚠️ 只在页面还是**骨架**的时候才整页 render —— 用户已经在看内容时整页重建，
       会把他正在进行的那一下点击吞掉（真因见 pcSafeRedraw 上面的实测说明）。
       已经有内容就走局部重绘：只换横幅与内容区，Tab 条和面板骨架原地不动，
       视觉上也不闪一下。 */
    if(cur === 'pcinfo'){
      if(document.getElementById('pcBody')) pcSafeRedraw(pcRepaint);
      else render();
      return;
    }
    if(cur === 'home') syncHomeLive();
  }, wait);
}

/* 进忙碌态：按钮转圈换字 + Tab 条下一道扫光 + 内容区淡下去（旧数据还在） */
function pcBusyStart(){
  window.__pcBusy = true;
  const panel = document.querySelector('.pc-panel');
  if(panel) panel.classList.add('pc-busy');
  const b = document.getElementById('pcRefresh');
  if(b){
    b.classList.add('loading');
    b.setAttribute('aria-busy', 'true');
    const t = b.querySelector('.pc-btn-t');
    if(t) t.textContent = '读取中…';
  }
}
function pcBusyEnd(){
  window.__pcBusy = false;
  const panel = document.querySelector('.pc-panel');
  if(panel) panel.classList.remove('pc-busy');
  const b = document.getElementById('pcRefresh');
  if(b){
    b.classList.remove('loading');
    b.removeAttribute('aria-busy');
    const t = b.querySelector('.pc-btn-t');
    if(t) t.textContent = '重新读取';
  }
}

/* 局部重绘：只换横幅与内容区，Tab 条和面板骨架原地不动 */
function pcRepaint(){
  const hero = document.querySelector('.pc-hero');
  if(hero){
    const tmp = document.createElement('div');
    tmp.innerHTML = pcHeroHtml();
    const nh = tmp.firstElementChild;
    if(nh){ hero.replaceWith(nh); paintIcons(nh); }
  }
  const body = document.getElementById('pcBody');
  if(body){
    body.innerHTML = pcTabBody();
    paintIcons(body);
    body.classList.remove('pc-in'); void body.offsetWidth;   /* 强制重排让入场动画能重播 */
    body.classList.add('pc-in');
    pcAnimate(body);
  }
}

/* ── 通用工具 ── */
const miss = function(v){ return (v === undefined || v === null || v === '') ? PC_NA : String(v); };
const esc  = function(s){ return String(s == null ? '' : s).replace(/[&<>"]/g, function(c){
  return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];
});};
/* 「值」这一格：缺失用灰色弱化，一眼看出这格没读到，不是在正常显示 */
const val = function(v){ const s = miss(v);
  return (s === PC_NA) ? '<span class="pc-na">'+PC_NA+'</span>' : esc(s);
};
const has = function(v){ return !(v === undefined || v === null || v === '' || v === PC_NA); };
const join = function(a, sep){ return a.filter(has).join(sep || ' · '); };

/* 一格字段。wide=true 跨两列 —— 长值（型号/序列号/明细）在 4 列窄格里会被折成
   两三行，白白吃掉垂直空间；跨列摊平反而更省高度 */
function kv(k, v, wide){ return '<div class="pc-kv'+(wide?' pc-kv-wide':'')+'"><span class="pc-k">'+esc(k)+'</span><span class="pc-v">'+val(v)+'</span></div>'; }
/* 强调字段：值加粗放大（品牌/型号这类最想让人看清的） */
function kvS(k, v, wide){ return '<div class="pc-kv pc-kv-strong'+(wide?' pc-kv-wide':'')+'"><span class="pc-k">'+esc(k)+'</span><span class="pc-v">'+val(v)+'</span></div>'; }
/* 整行字段：折行两三行以上的长值专用（支持模式 / 指令集 / 操作系统全名 / 长设备名）。
   宽度单调不小于跨两列 → 折行只会变少，绝不会变多，所以是纯改进。
   wide 参数不用了，但保留签名兼容旧调用。 */
function kvF(k, v){
  return '<div class="pc-kv pc-kv-full"><span class="pc-k">'+esc(k)+'</span>'
    + '<span class="pc-v">'+val(v)+'</span></div>';
}
/* 卡内分组小标题（左侧品牌渐变色条） */
function sec(title, note){
  return '<div class="pc-sec">'+esc(title)+(note ? '<span class="pc-sec-n">'+esc(note)+'</span>' : '')+'</div>';
}
/* 卡片。raw=false 时 inner 进两列网格，raw=true 时 inner 原样放 */
function block(title, ic, inner, hint, raw){
  return '<div class="card pc-card">'
    + '<div class="card-t"><span class="ct-ico">'+ico(ic,16)+'</span>'+esc(title)
    + (hint ? '<span class="pc-hint">'+esc(hint)+'</span>' : '') + '</div>'
    + (raw ? inner : '<div class="pc-grid">'+inner+'</div>')
    + '</div>';
}
/* 一格「标签 + 大数值」（主视觉右侧那排）。
   值太长会被省略号截断，所以 title 挂上原文，悬停能看全 —— 不为了排版把信息丢掉 */
function vstat(l, v, wide){
  return '<div class="pc-vstat'+(wide?' pc-vstat-wide':'')+'">'
    + '<span class="pc-vstat-l">'+esc(l)+'</span>'
    + '<span class="pc-vstat-v" title="'+esc(miss(v))+'">'+val(v)+'</span></div>';
}
/* 一张卡里塞多组内容（组用 sec 小标题分段）。
   为什么合并：每个独立卡要吃掉 30px 卡头 + 8px 间距，而这一页最缺的就是垂直空间
   （用户要求一屏装完）。合并后一张卡内部靠 sec 分段，层次反而更清楚。
   groups：[{name, note, inner, raw}] —— raw=true 的组（环形/磁盘这类）跨整行。 */
function pcBlock(ic, title, hint, groups){
  const inner = groups.map(function(g){
    const head = g.name ? sec(g.name, g.note) : '';
    return g.raw ? (head + '<div class="pc-sec-raw">' + g.inner + '</div>') : (head + g.inner);
  }).join('');
  return '<div class="card pc-card">'
    + '<div class="card-t"><span class="ct-ico">'+ico(ic,16)+'</span>'+esc(title)
    + (hint ? '<span class="pc-hint">'+esc(hint)+'</span>' : '') + '</div>'
    + '<div class="pc-grid">' + inner + '</div></div>';
}

/* ============================================================================
   图形件
   ============================================================================ */
/* 环形表盘：pct 与环长一一对应（不是装饰）。
   liveKey 非空时挂 data-live，每秒被 pcUpdateLive 刷新一次。 */
function donutPct(pct, label, liveKey){
  pct = Math.max(0, Math.min(100, pct));
  const C = 2 * Math.PI * 40, off = C * (1 - pct / 100);
  const col = pct >= 85 ? 'var(--danger)' : (pct >= 65 ? 'var(--warn)' : 'var(--info)');
  /* ⚠️ 颜色必须走 style=，不能写 stroke="var(--info)" 属性 ——
     SVG 的 presentation attribute **不解析 CSS 变量**，写属性上去环会整圈不见。
   ⚠️ 属性名必须带 pc 前缀（data-pclive / data-pccount）：
     主文件 1407 行的全局实时刷新会 `document.querySelectorAll('[data-live]')`
     并把 textContent 整个替换掉 —— 撞名的话环形表盘的内部结构会被抹成一段纯数字。 */
  return '<div class="pc-donut"'+(liveKey?' data-pclive="'+liveKey+'"':'')+'>'
    + '<div class="pc-donut-wrap">'
    +   '<svg viewBox="0 0 92 92" width="92" height="92">'
    +     '<circle class="pc-donut-bg" cx="46" cy="46" r="40"/>'
    +     '<circle class="pc-donut-fg" cx="46" cy="46" r="40" style="stroke:'+col+'"'
    +       ' stroke-dasharray="'+C.toFixed(1)+'" stroke-dashoffset="'+C.toFixed(1)+'"'
    +       ' data-circ="'+C.toFixed(1)+'" data-off="'+off.toFixed(1)+'"/>'
    +   '</svg>'
    +   '<div class="pc-donut-c"><span class="pc-donut-n" data-pccount="'+Math.round(pct)+'">0</span>'
    +     '<span class="pc-donut-u">%</span></div>'
    + '</div>'
    + (label ? '<div class="pc-donut-l">'+esc(label)+'</div>' : '')
    + '</div>';
}
/* 状态标签（磁盘介质、健康度这类） */
function tag(text, cls){
  if(!has(text)) return '';
  return '<span class="pc-tag'+(cls?' '+cls:'')+'">'+esc(text)+'</span>';
}

/* 磁盘介质/健康度 → 标签色（真实状态映射，不猜） */
function diskMediaTag(media, ssd){
  const isSsd = ssd || /固态|SSD/i.test(String(media || ''));
  return tag(isSsd ? '固态' : '机械', isSsd ? 'info' : '');
}
function diskHealthTag(status){
  if(!has(status)) return tag(PC_NA, '');
  if(/不良|异常|失败|警告|注意|重映射/i.test(status)) return tag(status, 'warn');
  if(/正常|良好|OK|健康/i.test(status)) return tag(status, 'ok');
  return tag(status, '');
}

/* ============================================================================
   ① Hero —— 机器名片
   ============================================================================ */
function pcHeroHtml(){
  const f = window.PC || {}, sp = f.spec || {}, fw = f.fw || {}, form = sp.form || {};

  const factor = form.factor || 'other';
  const brand  = sp.boardBrand || sp.sysVendor || '';
  const model  = sp.sysModel || '';
  const line1  = join([brand, model], ' ') || f.board || f.computer || '';
  const osLine = join([f.os, f.osVer]);

  /* 横幅只留「这台机器是什么」。撤掉的两条（用户 2026-10-10 明确）：
       · 「本次读取 2026-10-10 15:13:34」—— 用户不关心读的时刻，占一行
       · 「机型判断：机箱类型写着「台式机」」—— 机箱信息，同上
     留下的都是认机器用得上的：已开机、计算机名。 */
  const meta = [];
  if(f.uptime)  meta.push('已开机 ' + f.uptime);
  if(f.computer)meta.push('计算机 ' + f.computer);

  /* 固件徽标：状态点颜色 = 真实状态。
     ⚠️ 一字排开（BIOS / 启动模式 / 安全启动 / UEFI 证书），每枚一行内呈「点+名+值」。
     原来是 2×2 两行、名值上下叠，占 44px；现在一行 30px，省下的正好给内容区。
     ⚠️ 判定依据（如「检测到 Windows Boot Manager」）仍然只进 title 不占行。 */
  const badges = [];
  if(has(fw.biosVer) || has(fw.biosVendor)){
    badges.push({k:'BIOS', v: join([fw.biosVendor, fw.biosVer], ' '),
      dot:'info', tip: join([fw.biosDate, fw.smbios])});
  }
  if(has(fw.bootMode)){
    badges.push({k:'启动模式', v: fw.bootMode,
      dot: /uefi/i.test(fw.bootMode) ? 'info' : 'warn', tip: fw.bootDetail});
  }
  if(has(fw.secureBoot)){
    badges.push({k:'安全启动', v: fw.secureBoot,
      dot: fw.secureBoot === '开启' ? 'ok' : 'warn', tip:'开机时是否校验驱动与固件签名'});
  }
  if(has(fw.sbCerts)){
    badges.push({k:'UEFI 证书', v: fw.sbCerts, dot:'info', tip:'固件里已登记的启动证书数量'});
  }
  const badgeHtml = badges.length
    ? badges.map(function(b){
        return '<div class="pc-badge"'+(b.tip ? ' title="'+esc(b.tip)+'"' : '')+'>'
          + '<span class="pc-badge-d '+b.dot+'"></span>'
          + '<span class="pc-badge-k">'+esc(b.k)+'</span>'
          + '<span class="pc-badge-v">'+esc(b.v)+'</span></div>';
      }).join('')
    : '<div class="pc-badge pc-badge-na"><span class="pc-badge-d"></span>'
      + '<span class="pc-badge-k">固件</span>'
      + '<span class="pc-badge-v pc-na">'+PC_NA+'</span></div>';

  return '<div class="card pc-hero">'
    +   '<div class="pc-hero-fig">' + pcFormFigure(factor) + '</div>'
    +   '<div class="pc-hero-body">'
    +     '<div class="pc-hero-model" title="'+esc(line1)+'">'
    +       (has(line1) ? esc(line1) : '<span class="pc-na">'+PC_NA+'</span>') + '</div>'
    +     '<div class="pc-hero-os">' + (osLine ? esc(osLine) : '<span class="pc-na">'+PC_NA+'</span>') + '</div>'
    +     '<div class="pc-hero-meta">' + esc(meta.join(' · ') || PC_NA) + '</div>'
    +   '</div>'
    +   '<div class="pc-badges">' + badgeHtml + '</div>'
    + '</div>';
}

/* ============================================================================
   ② 右侧「实时状态」行 —— 一行一个指标（CPU / 内存 / 磁盘 / 温度）
   ---------------------------------------------------------------------------
   原来是四张独立大卡各自一张环，纵向叠到 404px，温度卡直接掉到折叠线以下
   （用户 2026-10-10：「温度这些怎么不显示呢？」—— 没显示是因为要滚才看得到）。
   现在合成**一张卡里的若干行**：环缩到 40px，一行 52px，五行也才 260px，
   温度跟其它指标并排，进页面就在眼前。
   ⚠️ 环的颜色/数值一一对应（不是装饰）；取不到的行不画 —— 0% 读起来像真测过。
   ============================================================================ */
function pcSideRow(o){
  const tint = o.pct === null || o.pct === undefined ? ''
    : (o.pct >= 85 ? 'rgba(251,113,133,.22)'
       : (o.pct >= 65 ? 'rgba(251,191,36,.22)' : 'rgba(61,107,245,.22)'));
  return '<div class="pc-srow"'+(o.key?' data-pcgauge="'+o.key+'"':'')
    + (tint ? ' style="--gc:'+tint+'"' : '')+'>'
    + '<span class="pc-srow-x">'
    +   '<span class="pc-srow-t">'+ico(o.ic,13)+'<span class="pc-srow-l">'+esc(o.l)+'</span>'
    +     '<b'+(has(o.main)?' title="'+esc(o.main)+'"':'')+'>'
    +       (has(o.main) ? esc(o.main) : '<span class="pc-na">'+PC_NA+'</span>')+'</b></span>'
    +   '<span class="pc-srow-s" title="'+esc(miss(o.sub))+'">'
    +     (has(o.sub) ? esc(o.sub) : '')+'</span>'
    + '</span>'
    + (o.pct === null || o.pct === undefined ? '' : donutPct(o.pct, '', o.key))
    + '</div>';
}

/* 温度区：照实标「温度」，绝不冒称 CPU 温度 —— 固件不告诉我们那是谁的。
   取不到就写「未获取到」并说明原因，不画一个假的 0。 */
function pcSideTempRow(){
  const temps = (window.PC && window.PC.temps) || [];
  if(!temps.length){
    return pcSideRow({ic:'bolt', l:'温度', main:'', key:'',
      sub:'CPU / 主板真实温度需要签名内核驱动，用户态读不到'});
  }
  return pcSideRow({ic:'bolt', l:'温度',
    main: temps.map(function(t){ return Number(t.c).toFixed(1); }).join(' / ') + ' °C',
    sub: temps.map(function(t){ return t.name; }).join(' · ') + '（固件报的环境温度）',
    key:''});
}

/* ============================================================================
   四个 Tab 的内容
   ============================================================================ */
const PC_TABS = [
  {id:'sum', name:'概要',  ico:'list'},
  {id:'cpu', name:'处理器', ico:'cpu'},
  {id:'mem', name:'内存',  ico:'grid'},
  {id:'main',name:'主板',  ico:'layers'},
  {id:'gpu', name:'显卡',  ico:'monitor'},
  {id:'disk',name:'硬盘',  ico:'disk'},
  {id:'net', name:'网络',  ico:'globe'},
  {id:'sys', name:'系统',  ico:'gear'},
];

/* ============================================================================
   摘要页 —— 概览清单 + 实时指标列（2026-10-10 第三版）
   ---------------------------------------------------------------------------
   为什么改成「一行一项」：上一版为了塞进一屏，把字段压成 4 列网格 + 1px 行距，
   用户反馈「太密了、全挤在一起、进来要一目了然」。清单式排法天然疏朗（视线沿
   一条竖线往下扫即可），而且每行可点 → 进该部件所在 Tab 看全部参数，
   **清单即导航**，不必再摆一排重叠入口（首页铁律：同一件事只给一个入口）。
   ============================================================================ */
const PC_CHEV = '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor"'
  + ' stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M9 6l6 6-6 6"/></svg>';

/* 清单一行：图标徽章 + 标签 + 值（主体 + 补充说明）+ 箭头。
   主体与补充分两个元素：主体优先占宽（放不下才省略），补充随后 —— 这样长设备名
   也不会把有用的补充信息挤掉。 */
function pcOvRow(ic, k, main, sub, tab){
  return '<button class="pc-row" data-pcgo="'+tab+'" title="点这里看「'+esc(k)+'」的全部参数">'
    + '<span class="pc-row-ic">'+ico(ic,17)+'</span>'
    + '<span class="pc-row-k">'+esc(k)+'</span>'
    + '<span class="pc-row-v">'
    +   (has(main) ? '<b>'+esc(main)+'</b>' : '<b class="pc-na">'+PC_NA+'</b>')
    +   (has(sub) ? '<i title="'+esc(sub)+'">'+esc(sub)+'</i>' : '')
    + '</span>'
    + '<span class="pc-row-a">'+PC_CHEV+'</span>'
    + '</button>';
}

/* 右侧一列实时指标：**合成一张卡**，一行一个指标，最下面是温度。
   为什么合成一张：四张独立卡各有卡头+间距，纵向叠到 404px，温度卡掉到折叠线以下；
   合成后一张卡一行 52px，五行 260px，进页面全都在眼前（用户点名要看到温度）。 */
function pcSummarySide(){
  const f = window.PC || {}, sp = f.spec || {};
  const p = window.__perf || {};
  const disks = f.disks || [];

  /* 磁盘占用 = 所有分区「已用 / 总容量」累加（可算的真实值，不是估的） */
  let dUsed = 0, dTotal = 0;
  disks.forEach(function(d){ dTotal += (d.total || 0); dUsed += ((d.total || 0) - (d.free || 0)); });
  const diskPct = dTotal > 0 ? Math.round(dUsed / dTotal * 100) : null;

  const rows = [];
  if(typeof p.cpu === 'number' && p.cpu >= 0)
    rows.push({key:'cpu', ic:'cpu', l:'CPU', pct:p.cpu,
      main: sp.cpuBase || sp.cpuMHz || '', sub: sp.cpuModelCn || sp.cpuModel || f.cpu});
  if(typeof p.mem === 'number' && p.mem >= 0)
    rows.push({key:'mem', ic:'grid', l:'内存', pct:p.mem,
      main: f.memTotal,
      /* ⚠️ memUsedT 自带「已用」二字（后端 fmt.Sprintf("已用 %.0f%%")），
         这里再加前缀会变成「已用 已用 81%」 */
      sub: join([f.memUsedT, sp.memType, sp.memSpeed])});
  if(typeof p.gpu === 'number' && p.gpu >= 0)
    rows.push({key:'gpu', ic:'monitor', l:'显卡', pct:p.gpu,
      main: sp.vram || '', sub: (f.gpus || [])[0]});
  if(diskPct !== null)
    rows.push({key:'', ic:'disk', l:'磁盘', pct:diskPct,
      main: dUsed > 0 ? (pcGb(dUsed) + ' / ' + pcGb(dTotal) + ' GB') : '',
      sub: disks.length ? (disks.length + ' 个分区') : ''});

  const body = rows.length
    ? rows.map(pcSideRow).join('')
    : '<div class="pc-side-na">CPU / 内存 / 显卡的实时占用率在这台机上未获取到</div>';

  return '<div class="card pc-side">'
    + '<div class="card-t"><span class="ct-ico">'+ico('bolt',16)+'</span>实时状态'
    +   '<span class="pc-hint">每秒刷新</span></div>'
    + '<div class="pc-srows">' + body + pcSideTempRow() + '</div>'
    + '</div>';
}

/* ---------- 摘要 ---------- */
function pcTabSummary(){
  const f = window.PC || {}, sp = f.spec || {}, fw = f.fw || {};
  const disks = f.disks || [], phys = f.physDisks || [];
  const gpus = f.gpus || [], brands = f.gpuBrands || [];
  const nets = f.nets || [], sounds = f.sounds || [], disp = f.display || [];

  /* 「主硬盘」= 系统盘所在的那块物理盘（sysIndex 命中 1）。命中不了就退第一块 ——
     只是换个说法，不编数据。 */
  let sysDisk = null;
  phys.forEach(function(p){ if(!sysDisk && String(p.sysIndex || '').indexOf('1') >= 0) sysDisk = p; });
  if(!sysDisk) sysDisk = phys[0] || null;

  const modes = disp[1] ? String(disp[1]).split('·').length : 0;

  const rows = [
    pcOvRow('cpu', 'CPU', sp.cpuModelCn || sp.cpuModel || f.cpu,
      join([sp.cpuCore, sp.cpuBase]), 'cpu'),
    pcOvRow('layers', '主板', f.board || sp.sysModel || sp.boardBrand || sp.sysVendor,
      join([sp.boardChip || f.chipset, sp.biosVer || fw.biosVer]), 'main'),
    pcOvRow('monitor', '显卡', gpus[0], join([sp.vram, brands[0]]), 'gpu'),
    pcOvRow('grid', '内存', f.memTotal,
      join([f.memUsedT, sp.memType, sp.memSpeed, sp.memChan]), 'mem'),
    pcOvRow('disk', '硬盘',
      sysDisk ? sysDisk.model : (disks[0] ? (disks[0].label || disks[0].name) : ''),
      sysDisk ? join([sysDisk.size, sysDisk.media, sysDisk.iface]) : '', 'disk'),
    /* 摘要行只放分辨率的头一段（「1920 × 1080 @ 60 Hz」），
       后面的「· 32 位色」对概览没用，整串塞进去反而被截断 */
    pcOvRow('max', '显示器', String(disp[0] || '').split(' · ')[0],
      modes > 1 ? ('支持 ' + modes + ' 档分辨率') : '', 'gpu'),
    pcOvRow('play', '声卡', sounds[0],
      sounds.length > 1 ? ('共 ' + sounds.length + ' 个音频设备') : '', 'net'),
    pcOvRow('wifi', '网卡', nets[0] ? nets[0].name : '',
      nets[0] ? join([nets[0].conn || nets[0].type, nets[0].speed, nets[0].mac]) : '', 'net'),
    pcOvRow('gear', '系统', join([f.os, f.osVer]),
      join([f.computer, f.install ? ('装于 ' + f.install) : '']), 'sys'),
  ];

  return '<div class="pc-ov">'
    + '<div class="pc-rows">' + rows.join('') + '</div>'
    + pcSummarySide()
    + '</div>';
}

/* ---------- 处理器 ---------- */
function pcTabCpu(){
  const f = window.PC || {}, sp = f.spec || {};
  const p = window.__perf || {};
  const occ = (typeof p.cpu === 'number' && p.cpu >= 0) ? p.cpu : null;

  const vis = '<div class="pc-vis">'
    + (occ !== null ? donutPct(occ, '当前占用', 'cpu')
                    : '<div class="pc-gauges-na">当前占用未获取到</div>')
    + '<div class="pc-vstats">'
    +   vstat('核心线程', sp.cpuCore) + vstat('基础频率', sp.cpuBase)
    +   vstat('最大睿频', sp.cpuTurbo) + vstat('三级缓存', sp.cpuCache)
    + '</div></div>';

  return pcBlock('cpu', '处理器', '占用每秒刷新 · 规格来自 CPUID / WMI', [
    {raw:true, inner: vis},
    {name:'规格', inner:
        kvS('品牌', sp.cpuBrand || f.cpu)
      + kvS('型号', sp.cpuModelCn || sp.cpuModel || f.cpu, true)
      + kv('当前实测频率', sp.cpuMHz)
      + kv('总线频率', sp.cpuBus)
      + kv('代号', sp.cpuCode)
      + kv('制造工艺', sp.cpuTech)
      + kv('插座', sp.cpuSocket)
      + kv('TDP', sp.cpuTdp)
      + kv('电压', sp.cpuVolt)
      /* 家族（Family 6 · Model 158 · Stepping 10 这类）跨两列刚好一行；
         指令集天生是一长串（MMX · SSE · SSE2 · … 共 9 项），整行放。
         这么分之后「电压 + 家族」正好凑满最后一行，不会剩一个孤字段。 */
      + kv('家族', sp.cpuFamily, true)
      + kvF('指令集', sp.cpuInstr)},
  ]);
}

/* ---------- 内存 ---------- */
function pcTabMem(){
  const f = window.PC || {}, sp = f.spec || {};
  const sticks = sp.memSticks || [];
  const used = (typeof f.memUsed === 'number' && f.memUsed >= 0) ? f.memUsed : null;

  /* ⚠️ 标签写「已用」时值必须只给数字百分比 ——
     f.memUsedT 本身就长成「已用 81%」（后端 fmt.Sprintf 拼好的），
     直接塞进来会显示成「已用 已用 81%」。 */
  const memPct = (typeof f.memUsed === 'number' && f.memUsed >= 0)
    ? (Math.round(f.memUsed) + '%') : '';

  const vis = '<div class="pc-vis">'
    + (used !== null ? donutPct(used, '已用', 'mem')
                     : '<div class="pc-gauges-na">内存占用未获取到</div>')
    + '<div class="pc-vstats">'
    +   vstat('总容量', f.memTotal) + vstat('已用', memPct)
    +   vstat('组成', f.memStick) + vstat('类型', sp.memType)
    + '</div></div>';

  return pcBlock('grid', '内存', '', [
    {raw:true, inner: vis},
    {name:'规格', inner:
        kv('品牌', sp.memBrand)
      + kv('标称频率', sp.memSpeed)
      + kv('实跑频率', sp.memUsedSpd)
      /* 标签由「通道」改成「内存条」：后端不再猜测通道数（SMBIOS 没有可靠字段），
         这里的值只是条数。继续叫「通道」会让用户把「4 条」误读成「四通道」。 */
      + kv('内存条', sp.memChan)
      + kv('插槽', sp.memSlots)
      + kv('工作电压', sp.memVolt)
      + kv('最大支持', sp.memMaxCap)},
    sticks.length ? {name:'内存条明细', note: sticks.length + ' 条',
      inner: sticks.map(function(s, i){
        /* 跨两列：单条明细「4 GB · DDR4 · 2400 MHz」在一列里会把 MHz 折到下一行 */
        return kvS('第 ' + (i + 1) + ' 条', join([s.size, s.manufacturer, s.type, s.speed]), true);
      }).join('')} : null,
  ].filter(Boolean));
}

/* ---------- 主板 ---------- */
function pcTabMain(){
  const f = window.PC || {}, sp = f.spec || {}, fw = f.fw || {};
  const vis = '<div class="pc-vis"><div class="pc-vstats">'
    + vstat('主板厂商', sp.boardBrand || sp.sysVendor)
    + vstat('主板型号', sp.sysModel || f.board)
    + vstat('芯片组', sp.boardChip || f.chipset)
    + vstat('BIOS 版本', fw.biosVer || sp.biosVer)
    + '</div></div>';
  return pcBlock('layers', '主板与固件', '现读', [
    {raw:true, inner: vis},
    {name:'详细信息', inner:
        kv('整机型号', join([sp.boardBrand || sp.sysVendor, sp.sysModel]), true)
      + kv('主板', f.board, true)
      + kv('版本', sp.boardVer)
      + kv('序列号', sp.boardSerial)
      + kv('SMBIOS', sp.smbiosVer || fw.smbios)
      + kv('BIOS 厂商', fw.biosVendor || sp.biosVendor, true)
      + kv('BIOS 版本', fw.biosVer || sp.biosVer)
      + kv('BIOS 日期', fw.biosDate || sp.biosDate)},
  ]);
}

/* ---------- 显卡 ---------- */
function pcTabGpu(){
  const f = window.PC || {}, sp = f.spec || {}, vrams = f.vrams || [], disp = f.display || [];
  const gpus = f.gpus || [], brands = f.gpuBrands || [];
  if(!gpus.length){
    return block('显卡', 'monitor', kv('显卡', ''), '这台机没读到显卡信息');
  }
  const groups = [];
  gpus.forEach(function(g, i){
    /* 显存按卡名匹配（DXGI 给「专用+共享」两个数，任务管理器同口径） */
    const low = String(g).toLowerCase();
    let v0 = null;
    vrams.forEach(function(v){
      if(v0) return;
      const n = String(v.name || '').toLowerCase();
      if(n && (low.indexOf(n) >= 0 || n.indexOf(low) >= 0)) v0 = v;
    });
    if(!v0) vrams.forEach(function(v){ if(!v0 && v.size) v0 = v; });
    groups.push({name: gpus.length > 1 ? ('显卡 ' + (i + 1)) : '', raw:true, inner:
      '<div class="pc-vis"><div class="pc-vstats">'
      + vstat('专用显存', v0 ? v0.size : sp.vram)
      + vstat('共享显存', v0 ? v0.sharedSize : '')
      + vstat('品牌', brands[i] || sp.gpuBrand)
      + vstat('接口', sp.gpuSlot)
      + '</div></div>'});
    groups.push({raw:true, inner:
      '<div class="pc-grid">'
      + kvS('型号', g, true)
      + kv('显存合计', sp.gpuVramAll)
      + kv('显存来源', v0 && v0.source ? pcSourceOne(v0.source) : '')
      + kv('驱动版本', sp.gpuDriver)
      /* 「1920 × 1080 @ 60 Hz · 32 位色」一列放不下会折两行，跨两列刚好一行 */
      + kv('当前分辨率', disp[0], true)
      /* 支持模式是 6 档并列（1920×1080@60 · 1680×1050@60 · …），塞窄格里会折 6 行，
         单独整行放 */
      + kvF('支持模式', disp[1])
      + '</div>'});
  });
  if(f.virts && f.virts.length){
    groups.push({name:'虚拟显示器', note:'没算进上面的真显卡', raw:true, inner:
      '<div class="pc-grid">'
      + kv('数量', f.virts.length + ' 个')
      + kv('明细', f.virts.join(' / '), true)
      + kv('说明', '远程/投屏/虚拟桌面用的，不是真显卡', true)
      + '</div>'});
  }
  return pcBlock('monitor', '显卡', '', groups);
}

/* ---------- 硬盘 ---------- */
function pcTabDisk(){
  const f = window.PC || {}, phys = f.physDisks || [], disks = f.disks || [];
  const groups = [];

  /* ① 分区占用：每分区一个环 —— 环比横条更能一眼看出「还剩多少」 */
  if(disks.length){
    const cards = disks.map(function(d){
      const t = d.total || 0, used = t - (d.free || 0);
      const pct = t > 0 ? Math.round(used / t * 100) : 0;
      const name = d.label || d.name || (d.drive + '（无卷标）');
      return '<div class="pc-disk">'
        + donutPct(pct, '', '')
        + '<div class="pc-disk-i">'
        +   '<div class="pc-disk-n" title="'+esc(name)+'">'+esc(name)+'</div>'
        +   '<div class="pc-disk-s">'+pcGb(used)+' / '+pcGb(t)+' GB · 已用 '+pct+'%</div>'
        +   '<div class="pc-disk-m">'+diskMediaTag('', d.ssd)+'<span class="pc-tag">'+esc(d.drive || '')+'</span></div>'
        + '</div></div>';
    }).join('');
    groups.push({name:'分区占用', note: disks.length + ' 个分区', raw:true,
      inner:'<div class="pc-disks">'+cards+'</div>'});
  }

  /* ② 物理硬盘：型号/接口/容量/介质/健康 */
  const multi = phys.length > 1;
  phys.forEach(function(p, i){
    groups.push({name: multi ? ('物理硬盘 ' + (i + 1)) : '物理硬盘', raw:true, inner:
      '<div class="pc-vis"><div class="pc-vstats">'
      + vstat('容量', p.size) + vstat('接口', p.iface)
      + vstat('介质', p.media) + vstat('健康', p.status)
      + '</div></div>'});
    /* 7 个次要字段压成 2 行（8 格）：序列号跨两列、型号只占一列。
       这么分是因为宽度需求不一样 —— 序列号是 20 位定长串，一列放不下会折成
       两行（实测 "907JPD2560000001912" 正好折在第 17 位），而型号一般 15 字符
       以内一列够用。原来 3 行 → 2 行，一台两块盘的机器省 52px。
       换台机若型号特别长（"Samsung SSD 980 PRO 1TB" 之类）最多折两行，
       退化成 3 行，不会比原来更差。 */
    groups.push({raw:true, inner:
      '<div class="pc-grid">'
      + kvS('型号', p.model)
      + kv('品牌', p.brand)
      + kv('序列号', p.serial, true)
      + kv('固件', p.fw)
      + kv('分区数', p.parts)
      + kv('盘符', p.letters)
      + kv('系统盘序号', p.sysIndex)
      + '</div>'});
  });

  if(!groups.length){
    return block('硬盘', 'disk', kv('物理硬盘', ''), '这台机未获取到硬盘信息');
  }
  return pcBlock('disk', '硬盘', '', groups);
}

/* ---------- 网络 ---------- */
function pcTabNet(){
  const f = window.PC || {}, nets = f.nets || [], sounds = f.sounds || [];
  /* 网卡 + 声卡合成一张卡（原来两张卡各占一个 30px 卡头，下面还各留半屏空 ——
     这台机就 1 个网卡 + 1 个声卡，合成一张后整页只剩一块，不再「一大个一大个」） */
  const groups = [];
  if(nets.length){
    nets.forEach(function(n, i){
      groups.push({name: nets.length > 1 ? ('网卡 ' + (i + 1)) : '网卡', raw:true, inner:
        '<div class="pc-vis"><div class="pc-vstats">'
        + vstat('连接方式', n.conn || n.type) + vstat('速率', n.speed)
        + vstat('状态', n.link) + vstat('品牌', n.brand)
        + '</div></div>'});
      const ipTxt = (n.ip && n.ip.length) ? n.ip.join(' / ') : '';
      groups.push({inner:
          kvS('名称', n.name, true)
        + kv('MAC', n.mac, true)
        + kv('类型', n.type)
        /* 多 IP（v4+v6 或双栈）才整行，单个 IP 跟「类型」并排就够 */
        + (n.ip && n.ip.length > 1 ? kvF('IP', ipTxt) : kv('IP', ipTxt))});
    });
  } else {
    groups.push({name:'网卡', inner: kv('网卡', '')});
    groups.push({inner: kv('说明', '这台机未获取到网卡信息')});
  }
  /* 声卡名普遍长（Realtek High Definition Audio / 英特尔 显示器音频 …），整行放 */
  groups.push({name:'声卡', inner:
      sounds.length ? kvF('声卡', sounds.join(' / ')) : kv('声卡', '')});
  return pcBlock('globe', '网络与音频', '', groups);
}

/* ---------- 系统 ---------- */
function pcTabSys(){
  const f = window.PC || {}, fw = f.fw || {};
  /* 操作系统全名（Microsoft Windows 10 专业版 · 10.0.19045 (64 位)）四格平分时
     会被省略号切掉尾巴，给它整行 —— 这一页底部本来就有富余高度 */
  const vis = '<div class="pc-vis"><div class="pc-vstats">'
    + vstat('操作系统', join([f.os, f.osVer]), true)
    + vstat('安装日期', f.install)
    + vstat('已开机', f.uptime)
    + vstat('启动模式', fw.bootMode)
    + '</div></div>';
  return pcBlock('gear', '系统与固件', '', [
    {raw:true, inner: vis},
    {name:'系统信息', inner:
        kv('计算机名', f.computer)},
    {name:'固件与启动', inner:
        kvF('BIOS 厂商', fw.biosVendor)
      + kv('BIOS 版本', fw.biosVer)
      + kv('BIOS 日期', fw.biosDate)
      + kv('SMBIOS', fw.smbios)
      + kv('启动模式', fw.bootMode)
      + kv('判定依据', fw.bootDetail, true)
      + kv('安全启动', fw.secureBoot)
      + kv('UEFI 证书', fw.sbCerts)},
  ]);
}

/* ============================================================================
   渲染
   ============================================================================ */
function pcTabBody(){
  switch(window.pcTab){
    case 'cpu':  return pcTabCpu();
    case 'mem':  return pcTabMem();
    case 'main': return pcTabMain();
    case 'gpu':  return pcTabGpu();
    case 'disk': return pcTabDisk();
    case 'net':  return pcTabNet();
    case 'sys':  return pcTabSys();
    default:     return pcTabSummary();
  }
}

function renderPc(){
  if(window.pcState === 'failed'){
    return '<div class="page-head"><div class="page-title">本机配置</div>'
      + '<div class="page-sub">读取失败：' + esc(window.pcErr || '后端未连接') + '</div></div>';
  }
  if(window.pcState === 'loading' && !window.PC){
    return '<div class="page-head"><div class="page-title">本机配置</div>'
      + '<div class="page-sub">正在读取这台电脑的硬件…</div></div>'
      + '<div class="pc-skel"></div>';
  }
  const tabs = '<div class="pc-tabs" role="tablist" style="--pc-tab-i:'+
    Math.max(0, PC_TABS.map(function(t){ return t.id; }).indexOf(window.pcTab))+'">'
    + PC_TABS.map(function(t){
        const on = (t.id === window.pcTab);
        return '<button class="pc-tab'+(on?' on':'')+'" data-pctab="'+t.id+'" role="tab"'
          + ' aria-selected="'+(on?'true':'false')+'"><span class="pc-tab-i">'+ico(t.ico,15)+'</span>'
          + t.name + '</button>';
      }).join('')
    + '</div>';

  return '<div class="page-head pc-head">'
    +   '<div class="page-title">本机配置</div>'
    +   '<div class="pc-acts">'
    /* 按钮文案单独包一层 span：读取中要能把「重新读取」换成「读取中…」，
       直接 textContent 整个按钮会把图标一起抹掉。 */
    +     '<button class="btn sm" id="pcRefresh">'+ico('refresh',15)+'<span class="pc-btn-t">重新读取</span></button>'
    +     '<button class="btn sm" id="pcExport">'+ico('doc',15)+'<span class="pc-btn-t">导出配置</span></button>'
    +   '</div>'
    + '</div>'
    + pcHeroHtml()
    /* Tab 条与内容区合成**一个区块**（一个边框一张卡）：
       原来是上下两块各自带边框、中间还留 9px 缝，看着像两个不相干的东西；
       合成后是一块「带标签页的面板」，标签页在面板里切内容。 */
    + '<div class="pc-panel">'
    +   tabs
    +   '<div class="pc-body" id="pcBody">' + pcTabBody() + '</div>'
    + '</div>';
}

/* ── 切页（Tab 条 与 概览清单行 共用同一条路径） ── */
function pcGoTab(id){
  if(!id || window.pcTab === id) return;
  window.pcTab = id;
  /* 只换 Tab 与内容区，横幅不动 —— 横幅是"这台机器的身份证"，切页不该让它闪 */
  const bar = document.querySelector('.pc-tabs');
  if(bar){
    bar.querySelectorAll('[data-pctab]').forEach(function(b){
      const on = b.dataset.pctab === id;
      b.classList.toggle('on', on);
      b.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    const idx = PC_TABS.map(function(t){ return t.id; }).indexOf(id);
    bar.style.setProperty('--pc-tab-i', String(Math.max(0, idx)));
  }
  const body = document.getElementById('pcBody');
  if(body){
    body.innerHTML = pcTabBody();
    paintIcons(body);
    body.classList.remove('pc-in'); void body.offsetWidth;   /* 强制重排让动画能重播 */
    body.classList.add('pc-in');
  }
  /* ⚠️⚠️ 必须重绑交互 —— 这个 bug 实测复现过（2026-10-10）：
     切页时 #pcBody 的 innerHTML 被整个换掉，新长出来的清单行是**全新 DOM**，
     连 dataset.boundGo 标记也一起没了。不重绑的话「点一行进详情」只有
     *这一页第一次渲染* 时才有效 —— 切过一次 Tab 再回来，九行全部点不动
     （表现为「点了 > 没反应」，实际是处理器根本没绑上）。
     重绑交给 bindPcinfo（内部有 dataset.bound 防重，重复调用安全，
     末尾的 pcAnimate / pcStartLive 也是幂等的）。 */
  if(window.bindPcinfo) window.bindPcinfo();
  const c = document.getElementById('content');
  if(c) c.scrollTop = 0;
}

/* 划过清单某一行 → 右侧对应的指标卡高亮（两者讲的是同一个部件，是真实对应） */
function pcHintGauge(tab, on){
  document.querySelectorAll('#pcBody [data-pcgauge]').forEach(function(g){
    g.classList.toggle('on', !!on && g.dataset.pcgauge === tab);
  });
}

/* ── 交互绑定 ── */
function bindPcinfo(){
  /* ── 事件委托（2026-10-10 改）────────────────────────────────────────────
     原来是一个个元素绑 click，靠 dataset.boundGo 防重。这条路有个死结：
     只要某处重绘忘了补绑（切 Tab 就踩过一次：九行全点不动），按钮就是死的。
     改成挂在 #content 上 —— 这个容器只换 innerHTML，**元素本身永不重建**，
     所以里面怎么重绘都不用重绑，这一整类 bug 从根上消失。
     另外加一层 pointerdown / pointerup 兜底：click 会因为"按下与抬起之间
     DOM 被换掉"而干脆不派发（实测复现，见 pcSafeRedraw 的说明），
     而 pointer 事件是分别派发的，能抓住"用户确实按过这一下"这件事实。 */
  const box = document.getElementById('content');
  if(box && !box.dataset.pcDelegated){
    box.dataset.pcDelegated = '1';
    const hitOf = function(e){
      const t = e.target;
      return (t && t.closest) ? t.closest('[data-pcgo],[data-pctab]') : null;
    };
    const idOf = function(el){ return el ? (el.dataset.pcgo || el.dataset.pctab || '') : ''; };
    document.addEventListener('pointerdown', function(e){
      window.__pcPress = idOf(hitOf(e));
      window.__pcPressAt = {x:e.clientX, y:e.clientY, t:Date.now()};
    }, true);
    document.addEventListener('pointerup', function(e){
      const want = window.__pcPress || ''; window.__pcPress = '';
      const a = window.__pcPressAt; window.__pcPressAt = null;
      if(!want) return;
      /* 拖出去松手 = 用户自己反悔，不跳 */
      if(a && (Math.abs(e.clientX - a.x) + Math.abs(e.clientY - a.y)) > 12) return;
      if(idOf(hitOf(e)) === want){ pcGoTab(want); return; }
      /* 抬起时目标没了（重绘正好把这一块换掉了）→ 按"按下去的那一下"照跳。
         这一下是真实发生过的，不该因为界面重画就白点。 */
      if(!hitOf(e)){ pcGoTab(want); return; }
      /* 抬起时落在别的条目上 = 移开了，算取消 */
    }, true);
    box.addEventListener('click', function(e){
      const id = idOf(hitOf(e));
      if(id) pcGoTab(id);          /* pcGoTab 自带同 id 判重，兜底触发过也不会跳两次 */
    });
    /* 划过清单某一行 → 右侧对应指标高亮（两者讲的是同一个部件） */
    box.addEventListener('mouseover', function(e){
      const el = hitOf(e);
      if(el && el.dataset.pcgo) pcHintGauge(el.dataset.pcgo, true);
    });
    box.addEventListener('mouseout', function(e){
      const el = hitOf(e);
      if(el && el.dataset.pcgo) pcHintGauge('', false);
    });
  }
  const rf = document.getElementById('pcRefresh');
  if(rf && !rf.dataset.bound){
    rf.dataset.bound = '1';
    rf.addEventListener('click', function(){
      if(window.__pcBusy) return;                 /* 防连点：读取中再点没意义 */
      if(!hasGo()){ toast('后端未连接', 'warn'); return; }
      /* ⚠️ 这里**不清空 window.PC、不调 render()**：
         清空会让整页塌回骨架屏（横幅、Tab、清单全没了再长回来）—— 那是「闪一下」，
         不是「在读取」。现在保留旧数据，走忙碌态 + 局部重绘。 */
      pcBusyStart();
      loadPC();
    });
  }
  /* 导出配置：弹系统「另存为」，保存位置由用户自己选（2026-10-10 用户要求）。
     三个返回口径分开处理：CANCEL 不当失败报（取消不是错）。 */
  const cp = document.getElementById('pcExport');
  if(cp && !cp.dataset.bound){
    cp.dataset.bound = '1';
    cp.addEventListener('click', function(){
      if(!hasGo()){ toast('后端未连接', 'warn'); return; }
      callGo('ExportPCInfo').then(function(p){
        const s = String(p == null ? '' : p);
        if(s === 'CANCEL'){ toast('已取消导出', 'info'); return; }
        if(!s){ toast('导出失败', 'warn'); return; }
        if(s.indexOf('ERR:') === 0){ toast('导出失败：' + s.slice(4), 'warn'); return; }
        toast('已导出到：' + s, 'ok');
      }).catch(function(e){ toast('导出失败：' + e, 'danger'); });
    });
  }
  pcAnimate(document.getElementById('pcBody'));
  pcStartLive();
}

/* 入场动画：环从 0 画到目标、数字从 0 滚到目标、字段逐行错峰浮现 */
function pcAnimate(root){
  if(!root) return;
  requestAnimationFrame(function(){
    root.querySelectorAll('.pc-donut-fg').forEach(function(a){
      a.style.strokeDashoffset = a.dataset.off || '0';
    });
    root.querySelectorAll('[data-pccount]').forEach(function(el){
      const target = Number(el.dataset.pccount) || 0;
      el.textContent = String(target);
    });
    root.querySelectorAll('.pc-row, .pc-srow, .pc-kv').forEach(function(el, i){
      el.style.animationDelay = Math.min(i * 16, 420) + 'ms';
    });
  });
}

/* 实时刷新：只在 pcinfo 页 + 元素还在时更新（离开页面自动停） */
function pcStartLive(){
  if(window.__pcLiveTimer) return;
  window.__pcLiveTimer = setInterval(function(){
    if(cur !== 'pcinfo' || !document.getElementById('pcBody')){
      clearInterval(window.__pcLiveTimer); window.__pcLiveTimer = null; return;
    }
    const p = window.__perf || {};
    document.querySelectorAll('#pcBody [data-pclive]').forEach(function(d){
      const v = p[d.dataset.pclive];
      if(typeof v !== 'number' || v < 0) return;
      const pct = Math.max(0, Math.min(100, v));
      const ring = d.querySelector('.pc-donut-fg');
      if(ring){
        const C = Number(ring.dataset.circ) || 2 * Math.PI * 40;
        ring.style.strokeDashoffset = (C * (1 - pct / 100)).toFixed(1);
        ring.style.stroke = pct >= 85 ? 'var(--danger)' : (pct >= 65 ? 'var(--warn)' : 'var(--info)');
      }
      const n = d.querySelector('.pc-donut-n');
      if(n) n.textContent = String(Math.round(pct));
    });
  }, 1000);
}

/* 显存来源说人话 */
function pcSourceOne(src){
  if(!src) return '';
  const m = {smbios:'SMBIOS', nvidia:'NVIDIA 驱动', amd:'AMD 驱动', intel:'Intel 驱动',
             d3dkg:'DirectX', registry:'注册表', api:'接口'};
  const arr = Array.isArray(src) ? src : [src];
  return arr.map(function(s){ return m[String(s).toLowerCase()] || s; }).join(' / ');
}

/* 机型线稿：一句话让人认出台式机还是笔记本（V2 原版沿用）。
   判不出机型时只画一台普通显示器 —— 不假装知道。 */
function pcFormFigure(factor){
  const desk = '<svg class="pc-fig" viewBox="0 0 150 150" aria-hidden="true">'
    + '<rect x="6" y="34" width="70" height="50" rx="4"/>'
    + '<rect x="12" y="40" width="58" height="38" rx="2" class="dim"/>'
    + '<path d="M36 84h10v10H36z" class="dim"/><path d="M24 98h34"/>'
    + '<rect x="94" y="14" width="46" height="104" rx="4"/>'
    + '<circle cx="117" cy="30" r="5"/>'
    + '<rect x="103" y="48" width="26" height="4" rx="1.5" class="dim"/>'
    + '<rect x="103" y="58" width="26" height="4" rx="1.5" class="dim"/>'
    + '<path d="M106 76v20M113 76v20M120 76v20M127 76v20" class="dim"/>'
    + '<path d="M98 126h38M104 126v6M130 126v6"/></svg>';
  const lap = '<svg class="pc-fig" viewBox="0 0 150 150" aria-hidden="true">'
    + '<rect x="24" y="16" width="102" height="66" rx="5"/>'
    + '<rect x="32" y="24" width="86" height="50" rx="2" class="dim"/>'
    + '<path d="M10 86h130l-10 14H20z"/>'
    + '<rect x="60" y="92" width="30" height="6" rx="2" class="dim"/>'
    + '<path d="M6 104h138"/></svg>';
  const tab = '<svg class="pc-fig" viewBox="0 0 150 150" aria-hidden="true">'
    + '<rect x="30" y="12" width="90" height="126" rx="11"/>'
    + '<rect x="38" y="24" width="74" height="98" rx="4" class="dim"/>'
    + '<circle cx="75" cy="131" r="4"/></svg>';
  const other = '<svg class="pc-fig" viewBox="0 0 150 150" aria-hidden="true">'
    + '<rect x="16" y="28" width="118" height="68" rx="6"/>'
    + '<rect x="26" y="38" width="98" height="48" rx="3" class="dim"/>'
    + '<path d="M66 96h18M75 96v16M56 112h38"/></svg>';
  return ({ desktop: desk, laptop: lap, tablet: tab })[factor] || other;
}

/* ── 页面切换时把内容顶回顶部 ── */
function pcEnter(){
  if(cur !== 'pcinfo') return;
  const c = document.getElementById('content');
  if(c) c.scrollTop = 0;
}

window.__pcinfo = {
  render: renderPc, bind: bindPcinfo, enter: pcEnter, load: loadPC,
  setTab: function(id){ if(window.pcTab !== id){ window.pcTab = id; } },
  getTab: function(){ return window.pcTab; }
};
window.renderPc = renderPc;
window.bindPcinfo = bindPcinfo;
window.loadPC = loadPC;
window.pcEnter = pcEnter;

})();
