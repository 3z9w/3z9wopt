package main

// 宽带测速（net-speed）
//
// 比常见测速工具多做的三件事：① 自动挑最快的节点（不是固定一个点，跨运营商更准）；
// ② 除上下行带宽外，还测延迟 / 抖动 / 丢包（打游戏、开会议卡不卡看这三个）；
// ③ 全程实时推送速率，界面上能看到速度爬升曲线，而不是干等一个最终数字。
//
// 节点都是国内可直连的公共大文件镜像 + Cloudflare 官方测速端点，不依赖任何第三方 SDK。
//
// v1.2.0 误报治理后的取舍（改动原因见《误报治理.md》§11）：
//   - 下载目标全部写死在代码里，**不再**从远端接口拉取节点列表——「远端下发 URL 当下载目标」
//     是杀软眼里最典型的下载器模板，宁可少几个专用节点也不背这个画像。
//   - 节点只用非可执行大文件（Ubuntu 镜像 / Cloudflare 计数端点），下载 exe 安装包的节点全部移除。
//   - UA 诚实标识本软件，不再伪装浏览器；不再带 Referer。
//   - 只允许重定向到白名单内的节点域名，远端无法把下载目标改到别的主机。
//   - 并发从 48 降到 8，回到普通多线程下载器的水平。

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// speedNode 一个候选测速节点
type speedNode struct {
	Name string // 展示名
	URL  string // 下行地址
	Host string // ping 用的主机名
	Up   string // 上行地址（可空 = 该节点不支持上行）
	Kind string // "range"（镜像大文件，用 Range 分段）或 "cf"（整文件反复 GET：Cloudflare / Ookla 节点）
}

// speedUA 测速请求的 User-Agent：诚实写自己的名字，不再伪装浏览器。
// 选节点时就按这个 UA 实测（见 speedProbe），能被拦的节点直接探测不到、自动落选。
var speedUA = "3z9wOpt/" + appVersion

// 内置候选节点：都是支持 Range 的大文件镜像或官方测速端点，全部为 HTTPS 静态地址。
// 想加节点就往这里加一条，探测阶段会自动算速率并选最快的。
var speedNodes = []speedNode{
	{Name: "华为云镜像", URL: "https://mirrors.huaweicloud.com/ubuntu-releases/22.04/ubuntu-22.04.5-desktop-amd64.iso", Host: "mirrors.huaweicloud.com", Kind: "range"},
	{Name: "网易镜像", URL: "https://mirrors.163.com/ubuntu-releases/22.04/ubuntu-22.04.5-desktop-amd64.iso", Host: "mirrors.163.com", Kind: "range"},
	{Name: "清华镜像", URL: "https://mirrors.tuna.tsinghua.edu.cn/ubuntu-releases/22.04/ubuntu-22.04.5-desktop-amd64.iso", Host: "mirrors.tuna.tsinghua.edu.cn", Kind: "range"},
	{Name: "Cloudflare", URL: "https://speed.cloudflare.com/__down?bytes=25000000", Host: "speed.cloudflare.com", Up: speedUpURL, Kind: "cf"},
}

// 上行兜底节点：国内镜像基本都不接受匿名 POST 大包，用 Cloudflare 官方上行端点
const speedUpURL = "https://speed.cloudflare.com/__up"

// 本机公网 IP 与归属地（ipip.net 公共接口：HTTPS、无需 key、中文地名）
const speedIPInfoURL = "https://myip.ipip.net/json"

const (
	speedProbeBytes  = 1200000  // 选节点时的探测流量（每节点约 1.2MB，并发探测约 3–4 秒）
	speedDownSeconds = 6        // 正式下行测速时长
	speedUpSeconds   = 4        // 上行测速时长
	speedDownThreads = 8        // 下行总并发连接（v1.2.0：48 → 8，普通多线程下载器水平）
	speedUpThreads   = 4        // 上行并发线程
	speedMaxBytes    = 600 << 20 // 下行总流量上限（保护流量，先到先停）
	speedPingCount   = 8        // 延迟测试包数（8 包约 6–8 秒，兼顾统计与总时长）
)

// speedAllowedHosts 允许访问 / 允许重定向过去的主机白名单：只含内置节点与两个固定端点。
// 目的只有一个——下载目标永远是我们写死在代码里的地址，远端没有任何途径把它改走。
var speedAllowedHosts = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range speedNodes {
		if h := strings.ToLower(n.Host); h != "" {
			m[h] = true
		}
	}
	for _, raw := range []string{speedUpURL, speedIPInfoURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			m[strings.ToLower(u.Hostname())] = true
		}
	}
	return m
}()

// speedRedirect 重定向策略：只许跳到白名单内的主机，最多 3 跳。
func speedRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("重定向次数过多")
	}
	host := strings.ToLower(req.URL.Hostname())
	if !speedAllowedHosts[host] {
		return fmt.Errorf("已阻止跳转到非测速节点：%s", host)
	}
	return nil
}

// ---- 事件推送 ----

// speedTick 把进度推给前端（speed:tick 事件，JSON 字符串）
func speedTick(tc *toolCtx, m map[string]interface{}) {
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tc.a.emit("speed:tick", string(b))
}

// ---- 主流程 ----

func toolSpeedTest(tc *toolCtx) (int, string) {
	tc.log("head", "宽带测速：自动选节点 → 延迟/抖动/丢包 → 下行 → 上行")

	// ① 选节点：只用内置的静态节点（下载地址全部写死在代码里，不做任何远端拉取）
	speedTick(tc, map[string]interface{}{"phase": "probe", "label": "正在挑选最快的测速节点…", "pct": 5})
	cands := append([]speedNode{}, speedNodes...)
	picks := speedPickNode(tc.ctx, cands, tc.log)
	if len(picks) == 0 {
		tc.log("err", "所有测速节点都连不上，请检查网络是否正常（或被防火墙拦截）")
		return 1, "测速失败：无可用节点"
	}
	nodeNames := make([]string, len(picks))
	nodes := make([]speedNode, len(picks))
	for i, p := range picks {
		nodeNames[i] = p.node.Name
		nodes[i] = p.node
	}
	nameAll := strings.Join(nodeNames, " + ")
	tc.log("ok", fmt.Sprintf("已选定测速节点：%s（前 %d 名聚合下载，探测 %s %.1f Mbps）", nameAll, len(nodes), picks[0].node.Name, picks[0].mbps))
	speedTick(tc, map[string]interface{}{"phase": "probe", "label": "已选定节点：" + nameAll, "node": nameAll, "pct": 12})

	// ② 延迟 / 抖动 / 丢包
	speedTick(tc, map[string]interface{}{"phase": "ping", "label": "正在测延迟、抖动与丢包…", "node": nameAll, "pct": 18})
	lat, jit, loss := speedPing(tc.ctx, picks[0].node.Host)
	if lat > 0 {
		tc.log("line", fmt.Sprintf("延迟 %.0f ms · 抖动 %.0f ms · 丢包 %.0f%%", lat, jit, loss))
	} else {
		tc.log("warn", "未能测出延迟（ping 被拦截？不影响带宽结果）")
	}
	speedTick(tc, map[string]interface{}{"phase": "ping", "label": "延迟测试完成", "node": nameAll,
		"lat": lat, "jitter": jit, "loss": loss, "pct": 26})

	// ③ 下行
	speedTick(tc, map[string]interface{}{"phase": "down", "label": "正在测下载速度…", "node": nameAll,
		"lat": lat, "jitter": jit, "loss": loss, "pct": 30})
	downPeak, downAvg, total := speedDownload(tc.ctx, nodes, func(mbps float64, pct float64) {
		speedTick(tc, map[string]interface{}{"phase": "down", "label": "正在测下载速度…", "node": nameAll,
			"lat": lat, "jitter": jit, "loss": loss, "mbps": mbps, "pct": pct})
	})
	if tc.ctx.Err() != nil {
		return -1, "已停止"
	}
	tc.log("ok", fmt.Sprintf("下载：峰值 %.1f Mbps · 平均 %.1f Mbps（约 %s）", downPeak, downAvg, humanMB(uint64(total))))
	speedTick(tc, map[string]interface{}{"phase": "down", "label": "下载测试完成", "node": nameAll,
		"lat": lat, "jitter": jit, "loss": loss, "mbps": downPeak, "down": downPeak, "downAvg": downAvg, "pct": 66})

	// ④ 上行
	speedTick(tc, map[string]interface{}{"phase": "up", "label": "正在测上传速度…", "node": nameAll,
		"lat": lat, "jitter": jit, "loss": loss, "down": downPeak, "downAvg": downAvg, "pct": 70})
	upPeak := speedUpload(tc.ctx, func(mbps float64, pct float64) {
		speedTick(tc, map[string]interface{}{"phase": "up", "label": "正在测上传速度…", "node": nameAll,
			"lat": lat, "jitter": jit, "loss": loss, "down": downPeak, "downAvg": downAvg, "mbps": mbps, "pct": pct})
	})
	if tc.ctx.Err() != nil {
		return -1, "已停止"
	}
	tc.log("ok", fmt.Sprintf("上传：峰值 %.1f Mbps", upPeak))

	tier, verdict := speedVerdict(downPeak, upPeak, lat, jit, loss)
	ip, loc, isp := speedIPInfo(tc.ctx)
	if ip != "" {
		tc.log("line", fmt.Sprintf("本机 IP %s · %s · %s", ip, loc, isp))
	}
	tc.log("head", "▶ 结论")
	tc.log("line", verdict)
	speedTick(tc, map[string]interface{}{"phase": "done", "label": "测速完成", "node": nameAll,
		"lat": lat, "jitter": jit, "loss": loss, "down": downPeak, "downAvg": downAvg, "up": upPeak,
		"tier": tier, "ip": ip, "loc": loc, "isp": isp,
		"verdict": verdict, "pct": 100})

	return 0, fmt.Sprintf("下行 %.1f Mbps · 上行 %.1f Mbps · 延迟 %.0f ms · 丢包 %.0f%%", downPeak, upPeak, lat, loss)
}

// ---- 选节点 ----

type probeResult struct {
	node speedNode
	mbps float64
}

func speedPickNode(ctx context.Context, cands []speedNode, logf func(kind, text string)) []probeResult {
	var (
		mu   sync.Mutex
		res  []probeResult
		wg   sync.WaitGroup
		done int32
	)
	for _, n := range cands {
		wg.Add(1)
		go func(n speedNode) {
			defer wg.Done()
			mbps := speedProbe(ctx, n)
			mu.Lock()
			res = append(res, probeResult{node: n, mbps: mbps})
			mu.Unlock()
			atomic.AddInt32(&done, 1)
			logf("dim", fmt.Sprintf("节点 %s 探测：%.1f Mbps", n.Name, mbps))
		}(n)
	}
	wg.Wait()
	sort.Slice(res, func(i, j int) bool { return res[i].mbps > res[j].mbps })
	// 只留连得上的，取最快的 6 个（不同源不同 IP，摊薄单源的总限速；360 同款思路的多点并发）
	var out []probeResult
	for _, r := range res {
		if r.mbps > 0 && len(out) < 6 {
			out = append(out, r)
		}
	}
	return out
}

func speedProbe(ctx context.Context, n speedNode) float64 {
	client := speedClient(1)
	req, err := http.NewRequestWithContext(ctx, "GET", n.URL, nil)
	if err != nil {
		return 0
	}
	if n.Kind == "range" {
		req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", speedProbeBytes-1))
	}
	req.Header.Set("User-Agent", speedUA)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		io.Copy(io.Discard, resp.Body)
		return 0
	}
	read := int64(0)
	buf := make([]byte, 64*1024)
	for read < speedProbeBytes {
		if ctx.Err() != nil {
			return 0
		}
		nr, err := resp.Body.Read(buf)
		read += int64(nr)
		if err != nil {
			break
		}
		if time.Since(start) > 4*time.Second {
			break
		}
	}
	sec := time.Since(start).Seconds()
	if sec <= 0 || read == 0 {
		return 0
	}
	return float64(read) * 8 / 1024 / 1024 / sec
}

// ---- 延迟 / 抖动 / 丢包 ----

var rePingTime = regexp.MustCompile(`(?i)(?:时间|time)[=<]\s*(\d+(?:\.\d+)?)\s*ms`)

// speedPing 用系统 ping 发 20 包：平均延迟、抖动（相邻包延迟差的平均）、丢包率。
// 静默执行（HideWindow + CREATE_NO_WINDOW），不会弹出黑色命令行窗口吓到用户。
func speedPing(ctx context.Context, host string) (lat, jitter, loss float64) {
	if ctx.Err() != nil {
		return 0, 0, 0
	}
	out, err := runHiddenOut(20*time.Second, "ping.exe", "-n", strconv.Itoa(speedPingCount), "-w", "1500", host)
	if err != nil {
		return 0, 0, 0
	}
	text := decodeConsole([]byte(out))
	var times []float64
	for _, m := range rePingTime.FindAllStringSubmatch(text, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			times = append(times, v)
		}
	}
	if len(times) == 0 {
		return 0, 0, 0
	}
	sum := 0.0
	for _, t := range times {
		sum += t
	}
	lat = sum / float64(len(times))
	if len(times) > 1 {
		d := 0.0
		for i := 1; i < len(times); i++ {
			d += math.Abs(times[i] - times[i-1])
		}
		jitter = d / float64(len(times)-1)
	}
	loss = (speedPingCount - float64(len(times))) / speedPingCount * 100
	if loss < 0 {
		loss = 0
	}
	return lat, jitter, loss
}

// ---- 下载测速 ----

func speedClient(maxIdle int) *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: speedRedirect, // 只许在节点白名单内跳转
		Transport: &http.Transport{
			DisableCompression:    true, // 关压缩：统计的是真实线路字节数
			MaxIdleConnsPerHost:   maxIdle + 2,
			MaxConnsPerHost:       0,
			ResponseHeaderTimeout: 15 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
		},
	}
}

func speedDownload(ctx context.Context, nodes []speedNode, onTick func(mbps, pct float64)) (peak, avg float64, total int64) {
	var counter int64
	deadline := time.Now().Add(speedDownSeconds * time.Second)
	client := speedClient(speedDownThreads)
	chunk := int64(16 << 20) // 每次请求 16MB（大块减少请求间隔开销，高带宽下更饱满）

	// 总并发均分到各节点（至少 4 连接/节点）：单节点限速时，多节点+高并发把带宽拉满
	per := speedDownThreads / len(nodes)
	if per < 4 {
		per = 4
	}
	var wg sync.WaitGroup
	for ni, n := range nodes {
		for i := 0; i < per; i++ {
			wg.Add(1)
			go func(n speedNode, idx int) {
				defer wg.Done()
				off := int64(ni)*1000*chunk + int64(idx)*100*chunk // 各线程起始错开，避免读到同一段被缓存命中
				for {
					if ctx.Err() != nil || time.Now().After(deadline) {
						return
					}
					if atomic.LoadInt64(&counter) > speedMaxBytes {
						return
					}
					req, err := http.NewRequestWithContext(ctx, "GET", n.URL, nil)
					if err != nil {
						return
					}
					if n.Kind == "range" {
						req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+chunk-1))
						off += chunk * int64(per)
					}
					req.Header.Set("User-Agent", speedUA)
					resp, err := client.Do(req)
					if err != nil {
						return
					}
					if resp.StatusCode >= 400 {
						resp.Body.Close()
						return
					}
					buf := make([]byte, 64*1024)
					for {
						if ctx.Err() != nil || time.Now().After(deadline) || atomic.LoadInt64(&counter) > speedMaxBytes {
							resp.Body.Close()
							return
						}
						nr, err := resp.Body.Read(buf)
						if nr > 0 {
							atomic.AddInt64(&counter, int64(nr))
						}
						if err != nil {
							break
						}
					}
					resp.Body.Close()
					if n.Kind == "cf" {
						// cf 端点每次固定大小，循环再来
						continue
					}
				}
			}(n, i)
		}
	}

	// 每秒采样一次，实时推速率
	var samples []float64
	last := int64(0)
	lastT := time.Now()
	for {
		if ctx.Err() != nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
		now := atomic.LoadInt64(&counter)
		el := time.Since(lastT).Seconds()
		if el <= 0 {
			continue
		}
		mbps := float64(now-last) * 8 / 1024 / 1024 / el
		last, lastT = now, time.Now()
		samples = append(samples, mbps)
		elapsed := time.Since(deadline.Add(-speedDownSeconds * time.Second)).Seconds()
		pct := 30 + math.Min(elapsed/float64(speedDownSeconds), 1)*36 // 30% → 66%
		onTick(mbps, pct)
		if time.Now().After(deadline) || now > speedMaxBytes {
			break
		}
	}
	wg.Wait()
	total = atomic.LoadInt64(&counter)
	// 峰值：跳过首个采样（含握手冷启动，偏低），取其后最大值
	if len(samples) > 1 {
		peak = samples[1]
		for _, s := range samples[1:] {
			if s > peak {
				peak = s
			}
		}
		sum := 0.0
		for _, s := range samples[1:] {
			sum += s
		}
		avg = sum / float64(len(samples)-1)
	} else if len(samples) == 1 {
		peak, avg = samples[0], samples[0]
	}
	return peak, avg, total
}

// ---- 上传测速 ----

func speedUpload(ctx context.Context, onTick func(mbps, pct float64)) float64 {
	payload := make([]byte, 1<<20) // 1MB 随机数据（随机避免被中间设备压缩）
	rand.Read(payload)
	var counter int64
	deadline := time.Now().Add(speedUpSeconds * time.Second)
	client := speedClient(speedUpThreads)

	var wg sync.WaitGroup
	for i := 0; i < speedUpThreads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil || time.Now().After(deadline) {
					return
				}
				req, err := http.NewRequestWithContext(ctx, "POST", speedUpURL, strings.NewReader(string(payload)))
				if err != nil {
					return
				}
				req.ContentLength = int64(len(payload))
				req.Header.Set("Content-Type", "application/octet-stream")
				req.Header.Set("User-Agent", speedUA)
				resp, err := client.Do(req)
				if err != nil {
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				atomic.AddInt64(&counter, int64(len(payload)))
			}
		}()
	}

	var samples []float64
	last := int64(0)
	lastT := time.Now()
	for {
		if ctx.Err() != nil {
			break
		}
		time.Sleep(400 * time.Millisecond)
		now := atomic.LoadInt64(&counter)
		el := time.Since(lastT).Seconds()
		if el > 0 {
			mbps := float64(now-last) * 8 / 1024 / 1024 / el
			last, lastT = now, time.Now()
			samples = append(samples, mbps)
			elapsed := time.Since(deadline.Add(-speedUpSeconds * time.Second)).Seconds()
			pct := 70 + math.Min(elapsed/float64(speedUpSeconds), 1)*28 // 70% → 98%
			onTick(mbps, pct)
		}
		if time.Now().After(deadline) {
			break
		}
	}
	wg.Wait()
	if len(samples) > 1 {
		peak := samples[1]
		for _, s := range samples[1:] {
			if s > peak {
				peak = s
			}
		}
		return peak
	}
	if len(samples) == 1 {
		return samples[0]
	}
	return 0
}

// ---- 结论 ----

// speedIPInfo 查本机公网 IP 与归属地（ipip.net 公共接口：HTTPS、无需 key、中文地名，失败静默）。
// v1.2.0 起由明文 http://ip-api.com 换成这里：不再走明文 HTTP，也不带任何本机信息上去。
func speedIPInfo(ctx context.Context) (ip, loc, isp string) {
	req, err := http.NewRequestWithContext(ctx, "GET", speedIPInfoURL, nil)
	if err != nil {
		return "", "", ""
	}
	req.Header.Set("User-Agent", speedUA)
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: speedRedirect}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	var m struct {
		Ret  string `json:"ret"`
		Data struct {
			IP       string   `json:"ip"`
			Location []string `json:"location"` // [国家, 省, 市, 区县, 运营商]
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil || m.Data.IP == "" {
		return "", "", ""
	}
	part := func(i int) string {
		if i < len(m.Data.Location) {
			return strings.TrimSpace(m.Data.Location[i])
		}
		return ""
	}
	return m.Data.IP, strings.TrimSpace(part(1) + " " + part(2)), part(4)
}

// speedVerdict 结论：tier = 套餐区间（界面刻度条大字），verdict = 一行结论
func speedVerdict(down, up, lat, jitter, loss float64) (string, string) {
	// 套餐区间：实测带宽通常略高于套餐标称（运营商超发），给出落在哪两档之间
	var tier string
	switch {
	case down >= 480:
		tier = "500M 以上"
	case down >= 235:
		tier = "200M-500M"
	case down >= 115:
		tier = "100M-200M"
	case down >= 60:
		tier = "50M-100M"
	case down >= 30:
		tier = "20M-50M"
	case down >= 15:
		tier = "10M-20M"
	default:
		tier = "10M 以下"
	}
	if down <= 0 {
		return tier, "下行测速失败，建议检查网络后重试"
	}
	// 一行结论：最大接入速度（MB/秒）+ 延迟 + 丢包 + 相当于什么宽带
	var b strings.Builder
	b.WriteString(fmt.Sprintf("最大接入速度 %.1f MB/秒", down/8))
	if lat > 0 {
		b.WriteString(fmt.Sprintf("，延迟 %.0f ms，丢包 %.0f%%", lat, loss))
	}
	b.WriteString("，相当于 " + tier + " 的宽带")
	return tier, b.String()
}

// humanMB 字节数转人类可读
func humanMB(b uint64) string {
	mb := float64(b) / 1024 / 1024
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.0f MB", mb)
}

