// main.go loomy2api-panel 入口：加载配置、构建账号池与面板、起 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/misakano7545/loomy2api-panel/internal/account"
	"github.com/misakano7545/loomy2api-panel/internal/config"
	"github.com/misakano7545/loomy2api-panel/internal/panel"
	"github.com/misakano7545/loomy2api-panel/internal/pool"
	"github.com/misakano7545/loomy2api-panel/internal/server"
	"github.com/misakano7545/loomy2api-panel/internal/upstream"
	"github.com/misakano7545/loomy2api-panel/internal/usage"
)

// appVersion 面板版本，发布构建由 CI 用 -X 注入（形如 2026.10.19.1430）。
var appVersion = "dev"

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（不存在时自动生成推荐配置）")
	flag.Parse()

	log.SetFlags(log.LstdFlags)

	loadedFrom := *cfgPath
	// Load 对「文件不存在」是宽容的（返回默认值并非错误），所以首启判断要显式做。
	if _, statErr := os.Stat(*cfgPath); errors.Is(statErr, fs.ErrNotExist) {
		if path, key, werr := config.Ensure(*cfgPath); werr == nil {
			log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", path, key)
		} else {
			log.Printf("config %s 不存在且自动生成失败（将用默认+环境变量）: %v", *cfgPath, werr)
			loadedFrom = ""
		}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if loadedFrom == "" {
		log.Printf("未读取配置文件，当前生效的是内置默认值 + 环境变量")
	}

	// ---- 账号池 ----
	stateFile := cfg.Resolve(cfg.StateFile)
	p := pool.New(stateFile)
	defer p.Close()

	authDir := cfg.Resolve(cfg.AuthDir)
	// 判死（清空登录态）必须立刻落盘：否则重启后会把一份已死的凭证又读回来，
	// 再去上游撞一次 401。池只认凭证内容，不知道目录在哪，所以由这里注入。
	p.SetAccountSaver(func(a *pool.Account) error { return pool.SaveAccount(authDir, a) })
	if accounts, lerr := pool.LoadAccounts(authDir); lerr == nil && len(accounts) > 0 {		p.SyncToDir(accounts)
		p.ApplyState()
		log.Printf("从 %s 加载 %d 个账号", authDir, len(accounts))
	} else if lerr != nil {
		log.Printf("读取账号目录 %s 失败: %v", authDir, lerr)
	} else {
		p.ApplyState()
	}
	p.SetOptions(panel.PoolOptions(cfg))

	// ---- 上游与账号服务客户端 ----
	upTimeout := time.Duration(cfg.TimeoutSecond) * time.Second
	up := upstream.NewClient(cfg.Upstream, cfg.LoomyVersion, cfg.Proxy, upTimeout)

	akID, akSecret := cfg.AccessKey()
	accClient := account.NewClient(cfg.AccountBase, cfg.AccountAppID, akID, akSecret, cfg.Proxy)

	// ---- 台账 ----
	usagePath := stateSibling(stateFile, "usage.jsonl")
	ledger := usage.New(usagePath, cfg.UsageMaxEntries)
	defer ledger.Close()
	ledger.Prune(30)

	// ---- 网关 ----
	h := server.New(cfg, p, up, ledger)
	server.Version = appVersion

	// ---- 面板 ----
	pl := panel.New(cfg, p, accClient, up, ledger, h.Models, appVersion)
	// log 包输出镜像进面板日志环：请求表格行（`| #001 |…`）按 chat 频道归类，
	// 面板「运行日志」因此能看到对话流水而不只是面板自己的动作。
	log.SetOutput(io.MultiWriter(os.Stderr, pl.LogWriter()))

	mux := http.NewServeMux()
	mux.Handle("/panel/", pl)
	mux.Handle("/", h.Routes())

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		// 入站无总读写超时：流式对话可能持续几十分钟。
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		log.Printf("Loomy2API 网关启动：http://%s  （面板 %s/panel/，版本 %s）",
			normalizeAddr(cfg.Listen), "http://"+normalizeAddr(cfg.Listen), appVersion)
		if len(cfg.APIKeys) == 0 {
			log.Printf("⚠️  api_key 为空：/v1/* 与 /panel/api/* 均不鉴权，请勿暴露到公网")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务退出: %v", err)
		}
	}()

	// 后台周期任务：额度刷新 + 会话粘性 GC + 自动续期提醒。
	stopCh := make(chan struct{})
	go background(cfg, p, up, pl, stopCh)

	// 模型目录预热：knownModel（裸 id 不补前缀）与图像模型拦截都依赖目录元数据，
	// 冷缓存下两者都会误判，而上游对认不出的模型名是静默回落到默认模型
	// （不报错、扣默认模型的积分）。异步执行，失败不阻塞启动。
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if n := h.WarmCatalog(ctx); n > 0 {
			log.Printf("模型目录预热：%d 个模型", n)
		}
	}()

	// ---- 优雅退出 ----
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在停止…")
	close(stopCh)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	p.Flush()
	_ = loadedFrom
}

// background 周期任务：额度刷新、粘性 GC、登录态役龄播报、登录态存疑复核。
func background(cfg *config.Config, p *pool.Pool, up *upstream.Client, pl *panel.Panel, stop <-chan struct{}) {
	quota := time.NewTicker(time.Duration(maxInt(cfg.QuotaRefreshMinutes, 1)) * time.Minute)
	defer quota.Stop()
	gc := time.NewTicker(5 * time.Minute)
	defer gc.Stop()
	// 复核跑得比额度刷新勤：存疑期间账号不接单，靠它尽快给结论。
	probe := time.NewTicker(30 * time.Second)
	defer probe.Stop()
	// noted 记录每个账号最近一次役龄播报的凭证签发时间，避免同一份 session 反复刷屏。
	noted := map[string]int64{}

	// 每日赠送按自然日清零，新一天的额度要一次 first-login（刷额度顺带做）才到账。
	// 固定 30 分钟的 ticker 会把这一轮排到 00:00–00:29 之间的任意时刻，那之前池子里
	// 每个号都「今天还没有赠送」，请求只能记在永久积分上（上游不报错、照常 200）。
	// 所以在跨日那一刻补一轮全量刷新：166 个号并发 4 大约 40 秒刷完，空窗从最长
	// 30 分钟压到几十秒。
	daily := time.NewTimer(untilNextDay(20 * time.Second))
	defer daily.Stop()

	for {
		select {
		case <-stop:
			return
		case <-gc.C:
			if n := p.Sticky().Prune(); n > 0 {
				log.Printf("会话粘性：清理 %d 条过期绑定", n)
			}
			noteSessionAge(p, cfg, noted)
		case <-probe.C:
			probeSuspectSessions(p, up)
		case <-quota.C:
			refreshQuotas(p, up)
		case <-daily.C:
			log.Printf("跨日：刷新全池额度（顺带 first-login 领新一天的每日赠送）")
			refreshQuotas(p, up)
			daily.Reset(untilNextDay(20 * time.Second))
		}
	}
}

// untilNextDay 距下一个自然日零点的时长（再加 after 的延迟，避开边界抖动）。
func untilNextDay(after time.Duration) time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
	return next.Sub(now) + after
}

// noteSessionAge 登录态役龄播报。
//
// 只报事实、不喊「请重新登录」：本地 expireAt 是按客户端申请值编出来的估计值
// （上游不返回过期时间），说「剩余 N 天」是假精确。役龄超过申请值只说明这份凭证
// 活得比客户端要求的久 —— 上游照常服务，唯一权威仍是 401。每份 session 只报一次。
func noteSessionAge(p *pool.Pool, cfg *config.Config, noted map[string]int64) {
	noteDays := cfg.SessionNoteDays
	if noteDays <= 0 {
		noteDays = 14
	}
	for _, a := range p.All() {
		if !a.Enabled || !a.HasSession() {
			continue
		}
		age := a.SessionAgeDays()
		if age < float64(noteDays) {
			continue
		}
		if noted[a.Name] == a.ObtainedAt {
			continue
		}
		noted[a.Name] = a.ObtainedAt
		how := "无密码，失效后只能短信重登"
		if a.Password != "" {
			how = "有密码，可点面板「登录」换新"
		}
		extra := ""
		if a.ObservedLifetime > 0 {
			extra = fmt.Sprintf("；此前实测寿命 ≥ %.1f 天", float64(a.ObservedLifetime)/86400)
		}
		log.Printf("登录态：账号 %s 已服役 %.1f 天（客户端申请 %d 天，本地到期日是估计值、不作判据%s），仍在上游正常使用，死活以上游 401 为准（%s）",
			a.Name, age, noteDays, extra, how)
	}
}

// probeSuspectSessions 复核「登录态存疑」的账号。
//
// 上游回过 401 后账号会出池等复核，这里用最便宜的一次读接口（/points/records）拿结论：
//   - 也回 401/403 → 凭证真死，清空登录态并记下实测寿命（无密码的号无法自救，所以要证据）；
//   - 其余结果（200 / 网络错误 / 5xx）→ 不下结论：200 就回池当抖动，其它让存疑窗口
//     自然到期回池，避免一次网络抖动把好号判死。
func probeSuspectSessions(p *pool.Pool, up *upstream.Client) {
	for _, a := range p.SuspectAccounts() {
		sess := a.Session
		if sess == "" {
			a.ClearSessionSuspect()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		status, body := up.ProbeSession(ctx, sess)
		cancel()

		if status == 0 { // 传输层失败：不下结论
			continue
		}
		if pool.Classify(status, body) == pool.FaultSession {
			life := ""
			if a.ObtainedAt > 0 {
				life = fmt.Sprintf("，该凭证实测寿命 %.1f 天", float64(time.Now().Unix()-a.ObtainedAt)/86400)
			}
			p.DropSession(a, fmt.Sprintf("登录态失效（上游 %d 拒绝，需重新登录）", status))
			log.Printf("账号 %s 登录态复核不通过（HTTP %d），已清空登录态%s", a.Name, status, life)
			continue
		}
		a.ClearSessionSuspect()
		log.Printf("账号 %s 登录态复核通过（HTTP %d），已回到轮换", a.Name, status)
	}
}

// refreshQuotas 并发刷新全池额度，并让余额恢复的账号自动解冻。
func refreshQuotas(p *pool.Pool, up *upstream.Client) {
	sem := make(chan struct{}, 4)
	done := make(chan struct{})
	count := 0
	for _, a := range p.All() {
		if !a.Enabled || !a.HasSession() {
			continue
		}
		count++
		go func(a *pool.Account) {
			defer func() { done <- struct{}{} }()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			q, err := up.Quota(ctx, a.Session)
			if err != nil {
				return
			}
			before := a.AvailablePoints()
			prevCycle := a.DailyCycleDate
			a.SetQuota(q)
			after := a.AvailablePoints()
			// Quota 内部已调幂等 first-login：跨日自动刷新每日积分。
			if q.CycleDate != "" && prevCycle != "" && q.CycleDate != prevCycle {
				log.Printf("账号 %s 进入新每日周期 %s（每日 %d，已自动签到刷新）", a.Name, q.CycleDate, derefI64(q.DailyQuota))
			}
			// 余额从耗尽恢复 → 自动解冻硬冷却。
			if before != nil && after != nil && *before <= 0 && *after > 0 {
				a.ResetCooldowns()
				log.Printf("账号 %s 余额恢复（%d），已自动解冻", a.Name, *after)
			}
		}(a)
	}
	for i := 0; i < count; i++ {
		<-done
	}
}

func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func normalizeAddr(addr string) string {
	if addr == "" {
		return "127.0.0.1:17890"
	}
	if addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

// derefI64 安全解引用 *int64（nil → 0），仅用于日志。
func derefI64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
