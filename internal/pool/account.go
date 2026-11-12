// Package pool 账号池：账号模型、选号、冷却/熔断、会话粘性、在途租约与状态持久化。
//
// 语义对齐 workbuddy2api-panel 的账号池，按 Loomy 的凭证形态重写：
//   - Loomy 没有 refresh token，登录态失效只能重新登录；
//     因此账号以「手机号 + 密码」为长期凭证，session 是可再生的工作凭证。
//   - 上游额度是积分（points）；余额耗尽属于硬冷却，等日额度重置。
//
// 登录态判据（对齐 loomy2api 的 v2 模型，2026-11-13 迁移）：
//   - 上游登录响应**不返回过期时间**，`expire_at` 是按客户端申请值（14 天）编出来的
//     估计值，只用于展示与役龄播报，**绝不参与路由判定**。旧的「本地到期就摘号」
//     会让上游还活着的凭证提前离开轮换（loomy-3147 实锤：本地过期 6 小时后仍可用）。
//   - 「手里有 session」才是本地上唯一的可用判据；凭证死活的唯一权威是上游 401/403。
//   - 一次 401 不等于判死（可能是抖动），先「存疑出池 + 轻量复核」，复核确认后才清
//     session —— 无密码的号被误清就无法自救，判死必须谨慎。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/loomy2api-panel/internal/account"
	"github.com/misakano7545/loomy2api-panel/internal/upstream"
)

// 冷却种类。
const (
	CoolNone = ""
	CoolSoft = "soft" // 限流类，有界指数退避
	CoolHard = "hard" // 额度耗尽，等到恢复墙钟
)

// Account 一个 Loomy 账号及其运行态。
type Account struct {
	Name    string `json:"name"`
	LoginID string `json:"loginid,omitempty"`
	// Remark 账号备注：纯展示，不参与选号 / 计费 / 鉴权。
	// 早期版本把它放在浏览器 localStorage，换浏览器、换入口（127.0.0.1 与局域网 IP
	// 是两个 origin）、清站点数据都会丢；落进 auths/<账号>.json 后与账号一起备份。
	Remark   string `json:"remark,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
	Paused   bool   `json:"paused,omitempty"` // 暂停选号：不参与转发，但照常签到/刷额度（保号）
	Proxy    string `json:"proxy,omitempty"`

	Session    string `json:"session,omitempty"`
	UserID     string `json:"userid,omitempty"`
	// ExpireAt 本地估计的登录态到期墙钟（= ObtainedAt + 客户端申请的 14 天）。
	// 上游没有下发过期时间，所以这是**编出来的估计值，只用于展示**，不作判据。
	ExpireAt   int64 `json:"expireAt,omitempty"`
	ObtainedAt int64 `json:"obtainedAt,omitempty"`
	// ObservedLifetime 实测登录态寿命（秒）：某份 session 被上游 401/403 拒绝时，
	// 记下「被拒那一刻的役龄」。这是唯一能学到真实 TTL 的时刻，落盘且只记一次
	// （字段名沿用 loomy2api 的 observedLifetime，两边凭证文件可以互搬）。
	ObservedLifetime int64 `json:"observedLifetime,omitempty"`

	Identity account.Identity `json:"identity,omitempty"`

	// 额度缓存（来自 /points/records + /points/first-login）
	Balance        *int64 `json:"balance,omitempty"`
	DailyBalance   *int64 `json:"daily_balance,omitempty"`
	Available      *int64 `json:"available,omitempty"`
	Permanent      *int64 `json:"permanent,omitempty"`
	DailyQuota     *int64 `json:"daily_quota,omitempty"`
	DailyUsed      *int64 `json:"daily_used,omitempty"`
	DailyCycleDate string `json:"daily_cycle_date,omitempty"`
	QuotaAt        int64  `json:"quota_updated_at,omitempty"`

	// 累计计数（落盘，重启不清零——用户对账要用）
	Requests   int64 `json:"requests"`
	PointsUsed int64 `json:"pointsUsed"`
	Failures   int64 `json:"failures"`

	// 运行态（不落盘的部分由 Runtime 承载）
	rt runtime
}

// runtime 非持久化的运行态。
type runtime struct {
	mu sync.Mutex

	cooldownUntil float64
	coolKind      string
	softStreak    int

	breakerUntil   float64
	breakerFails   int
	breakerRetries int

	modelCool map[string]modelCooldown

	lastUsed float64
	inflight int

	lastError     string
	dailyConsumed int64

	// suspectUntil 登录态存疑截止墙钟：上游回过 401、但复核还没结论。
	// 存疑期间不参与选号（Healthy 为假），窗口自然到期也会自动回池 ——
	// 复核本身失败（网络问题）不该把号永久关在外面。
	suspectUntil float64
	// suspectHits 连续「上游 401」次数（一次成功调用即清零）：累计到阈值直接判死，
	// 避免复核一直拿不到结论时反复出池-回池。
	suspectHits int
}

type modelCooldown struct {
	Until  float64 `json:"until"`
	Reason string  `json:"reason"`
	Hits   int     `json:"hits"`
}

// absorb 把另一份运行态搬过来（runtime 内含互斥锁，只能逐字段复制）。
// 调用方须持有目标与源的锁：这里只在池的写锁内、对刚构造的空 runtime 调用。
func (r *runtime) absorb(src *runtime) {
	src.mu.Lock()
	defer src.mu.Unlock()
	r.cooldownUntil = src.cooldownUntil
	r.coolKind = src.coolKind
	r.softStreak = src.softStreak
	r.breakerUntil = src.breakerUntil
	r.breakerFails = src.breakerFails
	r.breakerRetries = src.breakerRetries
	if len(src.modelCool) > 0 {
		r.modelCool = make(map[string]modelCooldown, len(src.modelCool))
		for k, v := range src.modelCool {
			r.modelCool[k] = v
		}
	}
	r.lastUsed = src.lastUsed
	r.inflight = src.inflight
	r.lastError = src.lastError
	r.dailyConsumed = src.dailyConsumed
	r.suspectUntil = src.suspectUntil
	r.suspectHits = src.suspectHits
}

// HasSession 手里是否有凭证 —— 本地上唯一的「能不能用」判据。
//
// 不看 expire_at：那是按客户端申请值编出来的估计值（见包注释）。真死只有一种表现，
// 就是上游 401/403 之后被 DropSession 清空。
func (a *Account) HasSession() bool {
	return a.Session != ""
}

// SessionAgeDays 当前凭证已服役天数（无凭证或不知签发时间返回 -1）。
//
// 面板与日志一律报「已服役 N 天」而不报「剩余 N 天」：前者是事实，后者是估计。
func (a *Account) SessionAgeDays() float64 {
	if a.Session == "" || a.ObtainedAt <= 0 {
		return -1
	}
	return float64(time.Now().Unix()-a.ObtainedAt) / 86400
}

// SessionStale 役龄是否已超过客户端申请的 14 天（**仅展示**，不参与任何判定）。
func (a *Account) SessionStale() bool {
	age := a.SessionAgeDays()
	return age >= 0 && age >= float64(account.SessionExpireSeconds)/86400
}

// DaysLeft 本地估计的登录态剩余天数（无估计返回 -1）。**仅展示**。
func (a *Account) DaysLeft() float64 {
	if a.ExpireAt == 0 {
		return -1
	}
	return float64(a.ExpireAt-time.Now().Unix()) / 86400
}

// InBreaker 是否处于熔断。
func (a *Account) InBreaker() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.breakerUntil > nowF()
}

// InCooldown 是否处于冷却（含熔断）。
func (a *Account) InCooldown() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.cooldownUntil > nowF() || a.rt.breakerUntil > nowF()
}

// Healthy 能否参与选号（不看额度）。
func (a *Account) Healthy() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	n := nowF()
	return a.Enabled && !a.Paused && a.HasSession() &&
		a.rt.cooldownUntil <= n && a.rt.breakerUntil <= n && a.rt.suspectUntil <= n
}

// ---------------------------------------------------------- 登录态存疑与判死

// MarkSessionSuspect 标记「登录态存疑」：上游刚回过 401，但结论未定。
//
// 存疑期间不参与选号，避免每一次客户端请求都去踩一遍这个号；窗口到期（或复核
// 通过 ClearSessionSuspect）自动回池。返回累计的连续 401 次数，供调用方决定
// 是否直接判死。
func (a *Account) MarkSessionSuspect(window float64, reason string) int {
	if window < 1 {
		window = 1
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.suspectHits++
	a.rt.suspectUntil = nowF() + window
	if reason != "" {
		a.rt.lastError = reason
	}
	return a.rt.suspectHits
}

// ClearSessionSuspect 复核通过：撤销存疑，回到轮换。
func (a *Account) ClearSessionSuspect() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.suspectUntil = 0
}

// InSessionSuspect 是否处于登录态存疑期。
func (a *Account) InSessionSuspect() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.suspectUntil > nowF()
}

// SessionSuspectLeft 存疑剩余秒数（0 = 不在存疑）。
func (a *Account) SessionSuspectLeft() float64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if d := a.rt.suspectUntil - nowF(); d > 0 {
		return d
	}
	return 0
}

// SessionSuspectHits 连续 401 次数（一次成功调用清零）。
func (a *Account) SessionSuspectHits() int {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.suspectHits
}

// RecordObservedLifetime 记下这份凭证的实测寿命（只记第一次被上游拒绝的那次）。
func (a *Account) RecordObservedLifetime() {
	if a.ObtainedAt <= 0 || a.ObservedLifetime > 0 {
		return
	}
	d := time.Now().Unix() - a.ObtainedAt
	if d < 0 {
		d = 0
	}
	a.ObservedLifetime = d
}

// DropSession 判死：清空凭证（立即退出轮换），保留账号本身与计数。
//
// 只有两种情况下调用：复核调用也被上游 401/403 拒绝，或连续 401 次数达阈值。
// 清空后账号只能靠人工重登（有密码可点面板「登录」，无密码只能短信重登），
// 所以这一步必须由证据支撑。
func (a *Account) DropSession(reason string) {
	a.RecordObservedLifetime()
	a.Session = ""
	a.ExpireAt = 0
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.suspectUntil = 0
	a.rt.cooldownUntil = 0
	a.rt.coolKind = ""
	a.rt.breakerUntil = 0
	a.rt.breakerFails = 0
	a.rt.lastError = reason
}

// HealthyForModel 模型感知的健康判定：被该模型限流的账号换模型仍可用。
func (a *Account) HealthyForModel(model string) bool {
	if !a.Healthy() {
		return false
	}
	return !a.ModelCooled(model)
}

// ModelCooled 该模型是否仍在冷却中。
func (a *Account) ModelCooled(model string) bool {
	if model == "" {
		return false
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	item, ok := a.rt.modelCool[model]
	if !ok {
		return false
	}
	if item.Until <= nowF() {
		delete(a.rt.modelCool, model)
		return false
	}
	return true
}

// Cool 冷却到 now+seconds（固定时长，不叠加）。
func (a *Account) Cool(seconds float64, kind, reason string) {
	if seconds < 1 {
		seconds = 1
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.cooldownUntil = nowF() + seconds
	a.rt.coolKind = kind
	if reason != "" {
		a.rt.lastError = reason
	}
}

// CoolUntil 冷却到指定墙钟（上游给了权威重置时间时用它）。
func (a *Account) CoolUntil(until float64, kind, reason string) {
	if until <= nowF() {
		until = nowF() + 0.001
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.cooldownUntil = until
	a.rt.coolKind = kind
	if reason != "" {
		a.rt.lastError = reason
	}
}

// CoolSoft 429/限流类软冷却，返回本次截止时间。
//
//   - 上游给了重置墙钟 → 对齐到该墙钟（截断到 max），不做指数放大；
//   - 没给 → 有界指数退避 base × 2^(streak-1)，封顶 max；
//   - 已在软冷却中再次命中 → 不推进 streak、不延长（防「越重试越冷」）。
func (a *Account) CoolSoft(baseSeconds, maxSeconds float64, reason string, resetAt float64) float64 {
	n := nowF()
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if resetAt > 0 {
		until := resetAt
		if lim := n + maxSeconds; until > lim {
			until = lim
		}
		if until <= n {
			until = n + 0.001
		}
		a.rt.cooldownUntil = until
		a.rt.coolKind = CoolSoft
		if reason != "" {
			a.rt.lastError = reason
		}
		return until
	}
	if a.rt.coolKind == CoolSoft && n < a.rt.cooldownUntil {
		return a.rt.cooldownUntil
	}
	a.rt.softStreak++
	shift := a.rt.softStreak - 1
	if shift > 6 {
		shift = 6
	}
	d := baseSeconds * float64(int64(1)<<uint(shift))
	if d < 1 {
		d = 1
	}
	if d > maxSeconds {
		d = maxSeconds
	}
	a.rt.cooldownUntil = n + d
	a.rt.coolKind = CoolSoft
	if reason != "" {
		a.rt.lastError = reason
	}
	return a.rt.cooldownUntil
}

// CoolHard 硬冷却（额度耗尽）。
func (a *Account) CoolHard(until float64, reason string) {
	a.CoolUntil(until, CoolHard, reason)
}

// CoolModel 记录一次模型级限流（换模型即可继续用）。
func (a *Account) CoolModel(model string, seconds float64, reason string) {
	if model == "" {
		return
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.rt.modelCool == nil {
		a.rt.modelCool = map[string]modelCooldown{}
	}
	item := a.rt.modelCool[model]
	item.Until = nowF() + seconds
	item.Reason = reason
	item.Hits++
	a.rt.modelCool[model] = item
}

// NoteError 记一次「账号自身」的失败，达阈值则熔断（逐次加倍，封顶 cap）。
func (a *Account) NoteError(threshold int, base, cap float64) bool {
	if threshold < 1 {
		threshold = 1
	}
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.breakerFails++
	if a.rt.breakerFails < threshold {
		return false
	}
	d := base * float64(int64(1)<<uint(minInt(a.rt.breakerRetries, 6)))
	if d > cap {
		d = cap
	}
	a.rt.breakerFails = 0
	a.rt.breakerRetries++
	a.rt.breakerUntil = nowF() + d
	return true
}

// NoteSuccess 成功一次：清熔断 + 清软冷却退避 + 清上次错误描述。
//
// lastError 是「当前状态」而非历史：这次调用既然成功了，之前那条错误
// 就不再描述该账号此刻的状态，留着会让面板一直挂一条早已过期的红字。
// 失败历史另有 Failures 计数与日志，不靠这个字段留档。
func (a *Account) NoteSuccess() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.breakerFails = 0
	a.rt.breakerRetries = 0
	a.rt.breakerUntil = 0
	a.rt.softStreak = 0
	if a.rt.coolKind == CoolSoft {
		a.rt.cooldownUntil = 0
		a.rt.coolKind = ""
	}
	// 这次请求真的成功了 = 登录态没问题，撤掉存疑与计数。
	a.rt.suspectUntil = 0
	a.rt.suspectHits = 0
	a.rt.lastError = ""
}

// ResetCooldowns 人工解冻：清掉全部冷却、熔断与上次错误（不动 disabled）。
func (a *Account) ResetCooldowns() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.cooldownUntil = 0
	a.rt.coolKind = ""
	a.rt.softStreak = 0
	a.rt.breakerUntil = 0
	a.rt.breakerFails = 0
	a.rt.breakerRetries = 0
	a.rt.modelCool = nil
	// 解冻意味着「已恢复正常」，留着解冻前那条错误只会误导。
	// 登录成功、余额恢复自动解冻也都走这里，所以错误描述随之自动消除。
	a.rt.lastError = ""
	// 登录态存疑同理：人工解冻 / 重登成功就是「这个号没问题」的表态。
	a.rt.suspectUntil = 0
	a.rt.suspectHits = 0
}

// Touch 记录一次使用。
func (a *Account) Touch() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.lastUsed = nowF()
}

// AcquireInflight 尝试占一个在途名额；limit<=0 表示不限。
func (a *Account) AcquireInflight(limit int) bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if limit <= 0 {
		a.rt.inflight++
		return true
	}
	if a.rt.inflight >= limit {
		return false
	}
	a.rt.inflight++
	return true
}

// ReleaseInflight 释放在途名额。
func (a *Account) ReleaseInflight() {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.rt.inflight > 0 {
		a.rt.inflight--
	}
}

// Inflight 当前在途数。
func (a *Account) Inflight() int {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.inflight
}

// LastUsed 最近使用时间（unix 秒，含小数）。
func (a *Account) LastUsed() float64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.lastUsed
}

// LastError 最近一次错误描述。
func (a *Account) LastError() string {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.lastError
}

// SetLastError 记录最近错误。
func (a *Account) SetLastError(msg string) {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	a.rt.lastError = msg
}

// AddPointsUsed 累加已耗积分。
func (a *Account) AddPointsUsed(n int64) {
	a.rt.mu.Lock()
	a.PointsUsed += n
	a.rt.dailyConsumed += n
	a.rt.mu.Unlock()
}

// DailyConsumed 当日本地累计消耗。
func (a *Account) DailyConsumed() int64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	return a.rt.dailyConsumed
}

// CheckinDone 今日是否已完成签到（额度快照的周期日 == 今天）。
// 上游口径：每日登录后 first-login 自动刷新，dailyCycleDate 即为已签到凭证。
func (a *Account) CheckinDone() bool {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.DailyCycleDate == "" {
		return false
	}
	return a.DailyCycleDate == time.Now().Format("2006-01-02")
}

// Public 面板/接口视图（无秘密）。
func (a *Account) Public() map[string]any {
	a.rt.mu.Lock()
	n := nowF()
	coolLeft := a.rt.cooldownUntil - n
	breakerLeft := a.rt.breakerUntil - n
	kind := a.rt.coolKind
	softStreak := a.rt.softStreak
	inflight := a.rt.inflight
	lastErr := a.rt.lastError
	dailyConsumed := a.rt.dailyConsumed
	modelCool := map[string]any{}
	for k, v := range a.rt.modelCool {
		if v.Until > n {
			modelCool[k] = map[string]any{
				"seconds_left": round1(v.Until - n),
				"reason":       v.Reason,
			}
		}
	}
	breakerFails := a.rt.breakerFails
	suspectLeft := a.rt.suspectUntil - n
	suspectHits := a.rt.suspectHits
	a.rt.mu.Unlock()

	sess := ""
	if a.Session != "" {
		// 不管长短一律只露前 8 位：短会话原样返回等于把凭证直接吐给浏览器。
		if len(a.Session) > 8 {
			sess = a.Session[:8] + "…"
		} else {
			sess = strings.Repeat("•", len(a.Session))
		}
	}
	out := map[string]any{
		"name":                  a.Name,
		"uid":                   a.Name, // 面板统一身份键（蓝本 workbuddy 用 uid，Loomy 用账号名）
		"nickname":              a.Name,
		"remark":                a.Remark,
		"loginid":               a.LoginID,
		"userid":                a.UserID,
		"enabled":               a.Enabled,
		"paused":                a.Paused,
		"disabled":              !a.Enabled,
		"has_password":          a.Password != "",
		"proxy":                 a.Proxy,
		"session":               sess,
		// session_valid = 手里还有凭证。真死（上游拒绝后清空）才会变 false；
		// 本地上「到期」不再是判据，所以它不再会因为役龄而翻成 false。
		"session_valid":         a.HasSession(),
		"session_days_left":     nil, // 本地估计的剩余天数（仅展示，见 session_age_days）
		"session_age_days":      nil, // 役龄：面板一律报这个，不报「剩余」
		"session_stale":         a.SessionStale(),
		"session_suspect":       suspectLeft > 0,
		"session_suspect_left":  maxF(round1(suspectLeft), 0),
		"session_suspect_hits":  suspectHits,
		"observed_lifetime_days": nil,
		"expire_at":             a.ExpireAt,
		"checkin_done":          a.CheckinDone(),
		"balance":               nil,
		"daily_balance":         nil,
		"available":             nil,
		"permanent":             nil,
		"daily_quota":           nil,
		"daily_used":            nil,
		"daily_cycle_date":      "",
		"quota_updated_at":      a.QuotaAt,
		"requests":              a.Requests,
		"points_used":           a.PointsUsed,
		"daily_consumed":        dailyConsumed,
		"failures":              a.Failures,
		"in_cooldown":           coolLeft > 0 || breakerLeft > 0,
		"cooldown_seconds_left": maxF(round1(coolLeft), 0),
		"cooldown_kind":         kind,
		"soft_streak":           softStreak,
		"in_breaker":            breakerLeft > 0,
		"breaker_seconds_left":  maxF(round1(breakerLeft), 0),
		"breaker_fails":         breakerFails,
		"model_cooldowns":       modelCool,
		"inflight":              inflight,
		"last_error":            lastErr,
		"identity": map[string]any{
			"devid":            a.Identity.DevID,
			"ua":               a.Identity.UA,
			"modelid":          a.Identity.ModelID,
			"version":          a.Identity.Version,
			"campus_device_id": clip(a.Identity.CampusDev, 32),
			"bound":            a.Identity.DevID != "",
		},
	}
	if d := a.DaysLeft(); d >= 0 {
		out["session_days_left"] = round2(d)
	}
	if age := a.SessionAgeDays(); age >= 0 {
		out["session_age_days"] = round2(age)
	}
	if a.ObservedLifetime > 0 {
		out["observed_lifetime_days"] = round2(float64(a.ObservedLifetime) / 86400)
	}
	if a.Balance != nil {
		out["balance"] = *a.Balance
	}
	if a.DailyBalance != nil {
		out["daily_balance"] = *a.DailyBalance
	}
	if a.Available != nil {
		out["available"] = *a.Available
	}
	if a.Permanent != nil {
		out["permanent"] = *a.Permanent
	}
	if a.DailyQuota != nil {
		out["daily_quota"] = *a.DailyQuota
	}
	if a.DailyUsed != nil {
		out["daily_used"] = *a.DailyUsed
	}
	out["daily_cycle_date"] = a.DailyCycleDate
	return out
}

// SetQuota 更新额度缓存（来自 upstream.Quota 的真实快照）。
func (a *Account) SetQuota(q *upstream.Quota) {
	if q == nil {
		return
	}
	a.rt.mu.Lock()
	a.Balance, a.DailyBalance, a.Available = q.Balance, q.Daily, q.Available
	a.Permanent, a.DailyQuota, a.DailyUsed = q.Permanent, q.DailyQuota, q.DailyUsed
	a.DailyCycleDate, a.QuotaAt = q.CycleDate, q.RefreshedAt
	a.rt.mu.Unlock()
}

// AvailablePoints 可用积分（未知返回 nil）。
func (a *Account) AvailablePoints() *int64 {
	a.rt.mu.Lock()
	defer a.rt.mu.Unlock()
	if a.Available != nil {
		return a.Available
	}
	// 上游没给 availableBalance 时用 balance+daily 估。
	if a.Balance == nil && a.DailyBalance == nil {
		return nil
	}
	var sum int64
	if a.Balance != nil {
		sum += *a.Balance
	}
	if a.DailyBalance != nil {
		sum += *a.DailyBalance
	}
	return &sum
}

// AccountFromMap 从持久化 JSON 还原账号。
func AccountFromMap(raw map[string]any, index int) *Account {
	name := strOf(raw["name"], raw["loginid"])
	if name == "" {
		name = "account" + itoa(index+1)
	}
	acc := &Account{
		Name:       name,
		LoginID:    strOf(raw["loginid"], raw["phone"]),
		Password:   strOf(raw["password"]),
		Proxy:      strOf(raw["proxy"]),
		Remark:     strOf(raw["remark"]),
		Session:    strOf(raw["session"]),
		UserID:     strOf(raw["userid"]),
		ExpireAt:   int64Of(raw["expireAt"], raw["expire_at"]),
		ObtainedAt: int64Of(raw["obtainedAt"], raw["obtained_at"]),
		// 兼容 loomy2api 的 observedLifetime（两边凭证文件可互搬）。
		ObservedLifetime: int64Of(raw["observedLifetime"], raw["observed_lifetime"]),
		Requests:         int64Of(raw["requests"]),
		PointsUsed: int64Of(raw["pointsUsed"], raw["points_used"]),
		Failures:   int64Of(raw["failures"]),
	}
	acc.Enabled = true
	if v, ok := raw["enabled"]; ok {
		acc.Enabled = toBool(v)
	}
	if v, ok := raw["paused"]; ok {
		acc.Paused = toBool(v)
	}
	if ident, ok := raw["identity"].(map[string]any); ok {
		acc.Identity = account.Identity{
			DevID:     strOf(ident["devid"]),
			UA:        strOf(ident["ua"]),
			ModelID:   strOf(ident["modelid"]),
			Version:   strOf(ident["version"]),
			CampusDev: strOf(ident["campus_device_id"]),
			CreatedAt: int64Of(ident["created_at"]),
		}
	}
	// 额度快照（见 persisted 注释：重启后不该归零）。
	acc.Balance = ptrFrom(raw, "balance")
	acc.DailyBalance = ptrFrom(raw, "daily_balance")
	acc.Available = ptrFrom(raw, "available")
	acc.Permanent = ptrFrom(raw, "permanent")
	acc.DailyQuota = ptrFrom(raw, "daily_quota")
	acc.DailyUsed = ptrFrom(raw, "daily_used")
	acc.DailyCycleDate = strOf(raw["daily_cycle_date"])
	acc.QuotaAt = int64Of(raw["quota_updated_at"])
	return acc
}

// ptrFrom 从落盘 JSON 里取可选整数（缺失返回 nil——「未知」与「0」必须能区分，
// 0 积分是真实状态，nil 是还没查过）。
func ptrFrom(raw map[string]any, key string) *int64 {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	n, ok := v.(float64)
	if !ok {
		return nil
	}
	i := int64(n)
	return &i
}

// ------------------------------------------------------------- persistence

// persisted 落盘的账号形态（只存长期凭证与计数，不存运行态）。
func (a *Account) persisted() map[string]any {
	out := map[string]any{
		"name":       a.Name,
		"enabled":    a.Enabled,
		"requests":   a.Requests,
		"pointsUsed": a.PointsUsed,
		"failures":   a.Failures,
	}
	if a.Paused {
		out["paused"] = true
	}
	if a.LoginID != "" {
		out["loginid"] = a.LoginID
	}
	if a.Password != "" {
		out["password"] = a.Password
	}
	if a.Proxy != "" {
		out["proxy"] = a.Proxy
	}
	// 备注必须写进快照：SaveAccount 是先 MarshalIndent 快照再整体覆盖文件，
	// 漏一个字段就会被下一次额度刷新（每 30 分钟全量 persist）静默冲掉。
	if a.Remark != "" {
		out["remark"] = a.Remark
	}
	if a.Session != "" {
		out["session"] = a.Session
	}
	if a.UserID != "" {
		out["userid"] = a.UserID
	}
	if a.ExpireAt != 0 {
		out["expireAt"] = a.ExpireAt
	}
	if a.ObtainedAt != 0 {
		out["obtainedAt"] = a.ObtainedAt
	}
	// 实测寿命必须落盘：这是唯一学到的真实 TTL，重启后还得用来指导补号节奏。
	if a.ObservedLifetime != 0 {
		out["observedLifetime"] = a.ObservedLifetime
	}
	if a.Identity.DevID != "" {
		out["identity"] = a.Identity
	}
	// 额度快照：面板的「积分 / 每日签到 / 周期日」全靠它，不落盘就会在重启后
	// 全部归零（面板显示 0 积分、未签到，直到下一次刷新）。上游查询很便宜但
	// 重启瞬间没有它，用户会以为额度丢了。
	a.rt.mu.Lock()
	for key, v := range map[string]*int64{
		"balance":       a.Balance,
		"daily_balance": a.DailyBalance,
		"available":     a.Available,
		"permanent":     a.Permanent,
		"daily_quota":   a.DailyQuota,
		"daily_used":    a.DailyUsed,
	} {
		if v != nil {
			out[key] = *v
		}
	}
	if a.DailyCycleDate != "" {
		out["daily_cycle_date"] = a.DailyCycleDate
	}
	if a.QuotaAt != 0 {
		out["quota_updated_at"] = a.QuotaAt
	}
	a.rt.mu.Unlock()
	return out
}

// LoadAccounts 读取账号目录（*.json，文件名即账号名兜底）。
func LoadAccounts(dir string) ([]*Account, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Account
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		// 支持单账号对象与账号数组两种形态。
		if arr, ok := m["accounts"].([]any); ok {
			for i, item := range arr {
				if am, ok := item.(map[string]any); ok {
					out = append(out, AccountFromMap(am, len(out)+i))
				}
			}
			continue
		}
		out = append(out, AccountFromMap(m, len(out)))
	}
	return out, nil
}

// SaveAccount 原子写回单个账号凭证（0600）。
func SaveAccount(dir string, a *Account) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(a.persisted(), "", "  ")
	if err != nil {
		return err
	}
	name := safeFileName(a.Name)
	if name == "" {
		name = safeFileName(a.LoginID)
	}
	if name == "" {
		name = "account"
	}
	target := filepath.Join(dir, name+".json")
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// RemoveAccountFile 删除账号凭证文件。
func RemoveAccountFile(dir string, name string) error {
	target := filepath.Join(dir, safeFileName(name)+".json")
	err := os.Remove(target)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func safeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == '@':
			b.WriteRune('_')
		}
	}
	return b.String()
}
