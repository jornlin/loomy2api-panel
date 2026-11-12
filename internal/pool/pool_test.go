package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestAccount(name string, available int64, enabled bool) *Account {
	v := available
	a := &Account{
		Name:      name,
		Enabled:   enabled,
		Session:   "sess-" + name,
		ExpireAt:  time.Now().Add(7 * 24 * time.Hour).Unix(),
		Available: &v,
	}
	return a
}

// TestWeightPrefersMoreCredits 积分多的账号权重必须更高。
func TestWeightPrefersMoreCredits(t *testing.T) {
	rich := newTestAccount("rich", 1000, true)
	poor := newTestAccount("poor", 100, true)
	max := 1000.0
	wr := weightOf(rich, max)
	wp := weightOf(poor, max)
	if wr <= wp {
		t.Fatalf("富账号权重应更高：%.2f vs %.2f", wr, wp)
	}
}

// TestWeightIdleCompensation 刚用过的账号权重更低（闲置补偿）。
func TestWeightIdleCompensation(t *testing.T) {
	fresh := newTestAccount("fresh", 500, true)
	stale := newTestAccount("stale", 500, true)
	fresh.rt.lastUsed = nowF()       // 刚用过
	stale.rt.lastUsed = nowF() - 600 // 久未使用（超过饱和时长）
	if weightOf(fresh, 500) >= weightOf(stale, 500) {
		t.Error("闲置久的账号权重应更高")
	}
}

// TestWeightNeverZero 权重恒 > 0，避免除零与全零抽签。
func TestWeightNeverZero(t *testing.T) {
	a := newTestAccount("a", 0, true)
	a.Available = nil
	if w := weightOf(a, 0); w <= 0 {
		t.Errorf("权重应 > 0，实际 %.4f", w)
	}
}

// TestWeightSuccessRate 成功率高的账号权重更高。
func TestWeightSuccessRate(t *testing.T) {
	good := newTestAccount("good", 500, true)
	good.Requests, good.Failures = 90, 10
	bad := newTestAccount("bad", 500, true)
	bad.Requests, bad.Failures = 10, 90
	if weightOf(good, 500) <= weightOf(bad, 500) {
		t.Error("成功率高者权重应更高")
	}
}

// TestPickWeightedAlwaysReturns Picker 必须总能返回一个候选（不返回 nil）。
func TestPickWeightedAlwaysReturns(t *testing.T) {
	var cands []*Account
	for i := 0; i < 6; i++ {
		cands = append(cands, newTestAccount(string(rune('a'+i)), int64(100*(i+1)), true))
	}
	for i := 0; i < 200; i++ {
		if got := pickWeighted(cands, 5); got == nil {
			t.Fatal("pickWeighted 返回 nil")
		}
	}
	if pickWeighted(nil, 5) != nil {
		t.Error("空候选应返回 nil")
	}
}

// TestPickWeightedSpreadsLoad 加权抽签不能让单个账号吃掉全部流量。
func TestPickWeightedSpreadsLoad(t *testing.T) {
	var cands []*Account
	for i := 0; i < 3; i++ {
		cands = append(cands, newTestAccount(string(rune('a'+i)), 100, true))
	}
	counts := map[string]int{}
	for i := 0; i < 600; i++ {
		got := pickWeighted(cands, 5)
		counts[got.Name]++
	}
	for _, a := range cands {
		if counts[a.Name] == 0 {
			t.Errorf("账号 %s 一次都没被选中（流量未分散）：%v", a.Name, counts)
		}
	}
}

// TestAcquireSkipsCooling 冷却中的账号不参与选号。
func TestAcquireSkipsCooling(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	p.Get("a").Cool(300, CoolSoft, "test")

	for i := 0; i < 50; i++ {
		acc, err := p.Acquire(nil, "m", "")
		if err != nil {
			t.Fatalf("应能选到 b：%v", err)
		}
		if acc.Name != "b" {
			t.Fatalf("选中了冷却中的账号 %s", acc.Name)
		}
		p.Release(acc)
	}
}

// TestAcquireAllCoolingFallsBack 全部冷却时仍挑一个顶班（不死锁 503）。
func TestAcquireAllCoolingFallsBack(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Get("a").Cool(300, CoolSoft, "test")
	acc, err := p.Acquire(nil, "m", "")
	if err != nil {
		t.Fatalf("全冷却时应能顶班选号：%v", err)
	}
	if acc == nil || acc.Name != "a" {
		t.Fatalf("顶班账号不对: %v", acc)
	}
	p.Release(acc)
}

// TestAcquireNoAccountWhenEmpty 没有账号时必须报错而非 panic。
func TestAcquireNoAccountWhenEmpty(t *testing.T) {
	p := New("")
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("空池应返回错误")
	}
}

// TestAcquireRespectsExclude 已失败过的账号不再被选中。
func TestAcquireRespectsExclude(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	for i := 0; i < 50; i++ {
		acc, err := p.Acquire([]string{"a"}, "m", "")
		if err != nil {
			t.Fatalf("应能选到 b：%v", err)
		}
		if acc.Name == "a" {
			t.Fatal("排除了 a 却选中 a")
		}
		p.Release(acc)
	}
}

// TestAcquireDisabledNotChosen 禁用账号不参与选号。
func TestAcquireDisabledNotChosen(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("off", 100, false))
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("只有禁用账号时应报无可用账号")
	}
}

// TestInflightLimit 单账号在途上限生效。
func TestInflightLimit(t *testing.T) {
	p := New("")
	p.SetOptions(Options{MaxInFlight: 1, PickTopN: 5, Strategy: "weighted",
		SoftRateBase: 600, SoftRateMax: 7200, NotFoundCool: 60, CooldownFallback: 300,
		BreakerThreshold: 5, BreakerBase: 60, BreakerMax: 1800,
		StickyTTL: 1800, StickyMaxEntries: 100})
	p.Add(newTestAccount("a", 100, true))

	first, err := p.Acquire(nil, "m", "")
	if err != nil {
		t.Fatalf("第一次应能选到：%v", err)
	}
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("在途已满时应报错")
	}
	p.Release(first)
	if _, err := p.Acquire(nil, "m", ""); err != nil {
		t.Fatalf("释放后应能再选到：%v", err)
	}
}

// TestStickyBindsNextAcquire 粘性命中的账号优先被复用。
func TestStickyBindsNextAcquire(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	key := "c-conv-1"
	p.Sticky().Bind(key, "b")

	for i := 0; i < 30; i++ {
		acc, err := p.Acquire(nil, "m", key)
		if err != nil {
			t.Fatalf("选号失败：%v", err)
		}
		if acc.Name != "b" {
			t.Fatalf("粘性命中却选中 %s", acc.Name)
		}
		p.Release(acc)
	}
}

// TestStickyUnbindOnFailure 失败后解绑，下次重新分配。
func TestStickyUnbindOnFailure(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 500, true))
	key := "c-conv-2"
	p.Sticky().Bind(key, "a")
	p.ReportFailure(p.Get("a"), 500, "boom", key, "m")
	if got := p.Sticky().Get(key); got != "" {
		t.Errorf("失败后应解绑，仍绑到 %q", got)
	}
}

// TestLocalExpiryDoesNotBlockRotation 本地估计的到期时间不作判据。
//
// 回归 loomy-3147：上游不返回过期时间，expireAt 是按客户端申请值（14 天）编出来的
// 估计值。上游的凭证往往活得比它久，拿它当门槛会把还活着的号提前摘出轮换
// （旧行为：11-21 一天摘掉 136 个号）。
func TestLocalExpiryDoesNotBlockRotation(t *testing.T) {
	p := New("")
	p.SetOptions(Options{Strategy: "round_robin", StickyTTL: 1800})
	a := newTestAccount("aged", 500, true)
	a.ExpireAt = time.Now().Add(-6 * time.Hour).Unix()  // 本地「已过期」
	a.ObtainedAt = time.Now().Add(-15 * 24 * time.Hour).Unix() // 已服役 15 天
	p.Add(a)

	if !a.HasSession() {
		t.Error("有凭证就该可用")
	}
	if !a.SessionStale() {
		t.Error("服役 15 天应标记为「超过客户端申请值」（仅展示用）")
	}
	if !a.Healthy() {
		t.Error("本地到期不该让账号不健康")
	}
	got, err := p.Acquire(nil, "m", "")
	if err != nil || got == nil || got.Name != "aged" {
		t.Fatalf("本地到期的号仍应参与轮换: acc=%v err=%v", got, err)
	}
}

// TestFaultSessionSuspectThenDrop 401 先存疑出池，复核确认后才判死。
func TestFaultSessionSuspectThenDrop(t *testing.T) {
	p := New("")
	p.SetOptions(Options{Strategy: "round_robin", SessionProbeSeconds: 120, SessionDropHits: 3})
	var saved int
	p.SetAccountSaver(func(a *Account) error { saved++; return nil })
	a := newTestAccount("a", 500, true)
	a.ObtainedAt = time.Now().Add(-15 * 24 * time.Hour).Unix()
	p.Add(a)
	healthy := newTestAccount("b", 500, true)
	p.Add(healthy)

	// 第一次 401：只存疑，凭证还在，但正常选号不再选它。
	p.ReportFailure(a, 401, "unauthorized", "", "m")
	if !a.HasSession() {
		t.Fatal("一次 401 不该判死（可能是抖动）")
	}
	if !a.InSessionSuspect() {
		t.Error("应进入存疑期")
	}
	if a.Healthy() {
		t.Error("存疑期间不该参与选号")
	}
	for i := 0; i < 10; i++ {
		got, err := p.Acquire(nil, "m", "")
		if err != nil {
			t.Fatalf("有健康账号时不该选不到: %v", err)
		}
		p.Release(got)
		if got.Name == "a" {
			t.Fatal("存疑账号不该被选中")
		}
	}
	if n := len(p.SuspectAccounts()); n != 1 {
		t.Errorf("存疑快照应有 1 个账号，实际 %d", n)
	}

	// 复核确认凭证已死。
	p.DropSession(a, "登录态失效（上游 401 拒绝，需重新登录）")
	if a.HasSession() {
		t.Error("判死后不该还留着凭证")
	}
	if a.Healthy() {
		t.Error("判死后不该可用")
	}
	if a.ObservedLifetime <= 0 || a.ObservedLifetime < int64(14*24*3600) {
		t.Errorf("应记录实测寿命，实际 %d", a.ObservedLifetime)
	}
	if saved == 0 {
		t.Error("判死必须立刻落盘")
	}
	if len(p.SuspectAccounts()) != 0 {
		t.Error("判死后不该还留在存疑快照里")
	}
}

// TestFaultSessionDropAfterHits 复核一直没结论时，连续 401 达阈值直接判死。
func TestFaultSessionDropAfterHits(t *testing.T) {
	p := New("")
	p.SetOptions(Options{SessionProbeSeconds: 120, SessionDropHits: 3})
	a := newTestAccount("a", 500, true)
	p.Add(a)

	for i := 1; i <= 2; i++ {
		p.ReportFailure(a, 401, "unauthorized", "", "m")
		if !a.HasSession() {
			t.Fatalf("第 %d 次 401 不该判死（阈值 3）", i)
		}
	}
	p.ReportFailure(a, 401, "unauthorized", "", "m")
	if a.HasSession() {
		t.Error("连续 3 次 401 应判死")
	}
}

// TestSuspectClearedBySuccess 复核窗口内客户端请求成功 = 凭证没问题，撤销存疑。
func TestSuspectClearedBySuccess(t *testing.T) {
	p := New("")
	p.SetOptions(Options{SessionProbeSeconds: 120, SessionDropHits: 3})
	a := newTestAccount("a", 500, true)
	p.Add(a)

	p.ReportFailure(a, 401, "unauthorized", "", "m")
	if a.LastError() == "" {
		t.Fatal("存疑时应给出描述")
	}
	a.ClearSessionSuspect() // 复核通过（server 侧调用）
	if !a.Healthy() {
		t.Error("撤销存疑后应回到轮换")
	}
	if a.LastError() != "" {
		t.Errorf("复核通过后不该还挂着「复核中」的描述，实际 %q", a.LastError())
	}
	p.ReportSuccess(a, "", "m", 0)
	if a.SessionSuspectHits() != 0 {
		t.Errorf("成功后连续 401 计数应清零，实际 %d", a.SessionSuspectHits())
	}
}

// TestObservedLifetimeRecordedOnce 实测寿命只记第一次，且能落盘/回读。
func TestObservedLifetimeRecordedOnce(t *testing.T) {
	dir := t.TempDir()
	a := newTestAccount("a", 500, true)
	a.ObtainedAt = time.Now().Add(-12 * 24 * time.Hour).Unix()

	a.DropSession("登录态失效")
	first := a.ObservedLifetime
	if first < int64(11*24*3600) || first > int64(13*24*3600) {
		t.Fatalf("实测寿命应在 12 天附近，实际 %d", first)
	}
	// 重新登录后再死一次：不得覆盖已学到的值。
	a.Session = "new"
	a.ObtainedAt = time.Now().Add(-time.Hour).Unix()
	a.DropSession("登录态失效")
	if a.ObservedLifetime != first {
		t.Errorf("实测寿命不该被覆盖：%d → %d", first, a.ObservedLifetime)
	}

	if err := SaveAccount(dir, a); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	back, err := LoadAccounts(dir)
	if err != nil || len(back) != 1 {
		t.Fatalf("回读失败: %v %d", err, len(back))
	}
	if back[0].ObservedLifetime != first {
		t.Errorf("实测寿命未落盘: %d", back[0].ObservedLifetime)
	}
}

// TestDailyTierRejectsStaleSnapshot 第 0 档（今天还有赠送额度）必须同时看余额和
// 周期日：每日赠送按自然日清零，昨天的余额今天一分不算，而新一天的额度要一次
// first-login 才到账 —— 只看余额会把流量主动送到「今天还没领到赠送」的号上。
func TestDailyTierRejectsStaleSnapshot(t *testing.T) {
	d, perm := int64(4000), int64(1000)
	stale := &Account{Name: "stale", DailyBalance: &d, DailyCycleDate: "2020-01-01", Permanent: &perm}
	none := &Account{Name: "none", DailyBalance: &d, Permanent: &perm} // 从没刷到过周期日
	fresh := &Account{Name: "fresh", DailyBalance: &d, DailyCycleDate: todayKey(), Permanent: &perm}

	if pickTier(stale) != 1 {
		t.Error("跨日的旧快照：赠送余额今天不算数，应退到「只剩永久」档")
	}
	if pickTier(none) != 1 {
		t.Error("周期日未知时不能当成今天还有赠送")
	}
	if pickTier(fresh) != 0 {
		t.Error("今天刷到过、余额为正 → 第 0 档")
	}
	got := preferDailyTier([]*Account{stale, none, fresh})
	if len(got) != 1 || got[0].Name != "fresh" {
		t.Fatalf("只应留下今天确实有赠送的号，实际 %v", names(got))
	}
}

// TestChargeDrainsDailyLocally 本地记账：一笔扣费既要在本地扣掉赠送余额，也要在
// 超出剩余赠送时把差额记到永久积分上 —— 这样两次额度刷新（30 分钟）之间就能发现
// 「这个号今天赠送用尽了」，立刻换号，而不是继续烧永久。
func TestChargeDrainsDailyLocally(t *testing.T) {
	dailyA, dailyB := int64(100), int64(5000)
	a := &Account{Name: "a", Enabled: true, Session: "s", DailyBalance: &dailyA, DailyCycleDate: todayKey()}
	b := &Account{Name: "b", Enabled: true, Session: "s", DailyBalance: &dailyB, DailyCycleDate: todayKey()}
	if pickTier(a) != 0 || pickTier(b) != 0 {
		t.Fatal("两个号今天都有赠送额度")
	}

	a.Charge(60)
	if got := a.DailyConsumed(); got != 60 {
		t.Errorf("当日消耗应记 60，实际 %d", got)
	}
	if *a.DailyBalance != 40 {
		t.Errorf("赠送余额应被本地扣到 40，实际 %d", *a.DailyBalance)
	}
	if a.PermBurnedToday() != 0 {
		t.Errorf("还没超支，不该记永久消耗，实际 %d", a.PermBurnedToday())
	}

	a.Charge(80) // 剩 40，超出 40 → 上游会改扣永久
	if *a.DailyBalance != 0 {
		t.Errorf("赠送余额应归零，实际 %d", *a.DailyBalance)
	}
	if got := a.PermBurnedToday(); got != 40 {
		t.Errorf("超出的 40 分应记在永久积分上，实际 %d", got)
	}
	if pickTier(a) != 2 {
		t.Errorf("赠送用尽且无永久余额 → 垫底档，实际 %d", pickTier(a))
	}

	// 端到端：赠送用尽的号立刻让位给还有赠送的号。
	p := New("")
	p.SetOptions(Options{Strategy: "round_robin", PreferDaily: true, StickyTTL: 0})
	p.Add(a)
	p.Add(b)
	for i := 0; i < 5; i++ {
		acc, err := p.Acquire(nil, "m", "")
		if err != nil {
			t.Fatalf("第 %d 次选号失败: %v", i, err)
		}
		p.Release(acc)
		if acc.Name != "b" {
			t.Fatalf("第 %d 次选到了 %s：赠送用尽的号不该再被选中", i, acc.Name)
		}
	}
}

// TestStickyYieldsToDailyTier 粘性绑定号当天赠送已用尽、池里还有别的号有赠送时，
// 不能继续粘着它烧永久积分：该让位给有赠送的号，并把绑定改写到新号上。
func TestStickyYieldsToDailyTier(t *testing.T) {
	p := New("")
	p.SetOptions(Options{Strategy: "round_robin", PreferDaily: true, StickyTTL: 1800, StickyMaxEntries: 10})
	z, big := int64(0), int64(5000)
	perm := int64(100000)
	drained := &Account{Name: "drained", Enabled: true, Session: "s1", DailyBalance: &z,
		DailyCycleDate: todayKey(), Permanent: &perm, Available: &perm}
	fresh := &Account{Name: "fresh", Enabled: true, Session: "s2", DailyBalance: &big,
		DailyCycleDate: todayKey(), Permanent: &perm, Available: &perm}
	p.Add(drained)
	p.Add(fresh)

	// 上一轮把这条会话粘在了「赠送已用尽」的号上
	p.Sticky().Bind("c-1", "drained")
	acc, err := p.Acquire(nil, "m", "c-1")
	if err != nil {
		t.Fatalf("选号失败: %v", err)
	}
	if acc.Name != "fresh" {
		t.Fatalf("应让位给今天还有赠送的号，实际选到 %s", acc.Name)
	}
	p.ReportSuccess(acc, "c-1", "m", 0)
	if got := p.Sticky().Get("c-1"); got != "fresh" {
		t.Errorf("绑定应改写到新号，实际 %q", got)
	}
	p.Release(acc)

	// 池里没有别的有赠送的号时，粘性照旧生效（不能因为分层而无号可用）
	p.Remove("fresh")
	acc2, err := p.Acquire(nil, "m", "c-1")
	if err != nil || acc2.Name != "drained" {
		t.Fatalf("无替代号时应继续用绑定号: acc=%v err=%v", acc2, err)
	}
	p.Release(acc2)
}

// TestDailyCountersResetAcrossDay 本地当日计数跨日归零（否则「今天消耗了多少」
// 会随着进程寿命一直累加，面板上的数就是假的）。
func TestDailyCountersResetAcrossDay(t *testing.T) {
	d := int64(10)
	a := &Account{Name: "a", DailyBalance: &d, DailyCycleDate: todayKey()}
	a.Charge(5)
	if a.DailyConsumed() != 5 {
		t.Fatalf("当日消耗应为 5，实际 %d", a.DailyConsumed())
	}

	a.rt.mu.Lock() // 伪造成昨天记的账
	a.rt.dayKey = "2020-01-01"
	a.rt.permBurned = 77
	a.rt.mu.Unlock()
	if a.DailyConsumed() != 0 || a.PermBurnedToday() != 0 {
		t.Error("跨日后两个当日计数都应读成 0")
	}
	a.Charge(3)
	if a.DailyConsumed() != 3 || a.PermBurnedToday() != 0 {
		t.Errorf("跨日后第一笔应重新起算：消耗 %d / 永久 %d", a.DailyConsumed(), a.PermBurnedToday())
	}
}

// TestLastErrorClearedOnSuccess 面板「状态」下的错误描述是当前状态：下一次
// 成功调用必须把它清掉，否则一条早已恢复的错误会一直挂在面板上。
func TestLastErrorClearedOnSuccess(t *testing.T) {
	p := New("")
	a := newTestAccount("a", 500, true)
	p.Add(a)

	// 客户端类失败（不罚账号、不冷却）：错误只体现在 last_error 上。
	p.ReportFailure(a, 400, "bad request", "", "m")
	if a.LastError() == "" {
		t.Fatal("失败后应记录 last_error")
	}

	p.ReportSuccess(a, "", "m", 0)
	if got := a.LastError(); got != "" {
		t.Errorf("成功后 last_error 应清空，仍有 %q", got)
	}

	// 人工解冻同样代表「已恢复」，不该留着解冻前的错误。
	p.ReportFailure(a, 500, "boom", "", "m")
	a.ResetCooldowns()
	if got := a.LastError(); got != "" {
		t.Errorf("解冻后 last_error 应清空，仍有 %q", got)
	}
}

// TestModelCooldownScopedToModel 模型级限流只锁该模型，换模型仍可用。
func TestModelCooldownScopedToModel(t *testing.T) {
	a := newTestAccount("a", 100, true)
	a.CoolModel("model-x", 300, "限流")
	if !a.ModelCooled("model-x") {
		t.Error("model-x 应处于冷却")
	}
	if a.ModelCooled("model-y") {
		t.Error("model-y 不应受影响")
	}
	if !a.Healthy() {
		t.Error("模型级冷却不应让账号整体不健康")
	}
	if a.HealthyForModel("model-x") {
		t.Error("model-x 上应判定为不可用")
	}
}

// TestSoftCooldownExponential 429 软冷却逐次加倍并封顶。
func TestSoftCooldownExponential(t *testing.T) {
	a := newTestAccount("a", 100, true)
	base, max := 100.0, 1000.0

	first := a.CoolSoft(base, max, "429", 0) - nowF()
	if first < base*0.9 || first > base*1.5 {
		t.Errorf("首次软冷却约 %v 秒，实际 %.1f", base, first)
	}
	a.NoteSuccess() // 清退避
	second := a.CoolSoft(base, max, "429", 0) - nowF()
	if second < base*0.9 {
		t.Errorf("退避不应低于基数：%.1f", second)
	}

	// 连续命中不得无限增长
	for i := 0; i < 20; i++ {
		a.NoteSuccess()
		got := a.CoolSoft(base, max, "429", 0) - nowF()
		if got > max*1.1 {
			t.Errorf("第 %d 次冷冷却 %.1f 超过封顶 %v", i, got, max)
		}
	}
}

// TestSoftCooldownHonorsResetAt 上游给了重置墙钟时对齐到该时间（截断到封顶）。
func TestSoftCooldownHonorsResetAt(t *testing.T) {
	a := newTestAccount("a", 100, true)
	reset := nowF() + 500
	got := a.CoolSoft(100, 1000, "429", reset)
	if diff := got - reset; diff > 1 || diff < -1 {
		t.Errorf("冷却截止 %.1f，期望对齐到 %.1f", got, reset)
	}

	// 重置时间远大于封顶 → 截断
	a2 := newTestAccount("b", 100, true)
	far := nowF() + 99999
	got2 := a2.CoolSoft(100, 1000, "429", far)
	if got2-nowF() > 1001 {
		t.Errorf("应截断到封顶 1000s，实际 %.1f", got2-nowF())
	}
}

// TestCoolSoftDoubleHitDoesNotExtend 冷却中再次命中不延长（防越重试越冷）。
func TestCoolSoftDoubleHitDoesNotExtend(t *testing.T) {
	a := newTestAccount("a", 100, true)
	first := a.CoolSoft(100, 1000, "429", 0)
	second := a.CoolSoft(100, 1000, "429", 0)
	if second != first {
		t.Errorf("冷却中重复命中不应延长：%.1f vs %.1f", first, second)
	}
}

// TestBreakerTripsAfterThreshold 连续失败达阈值后熔断。
func TestBreakerTripsAfterThreshold(t *testing.T) {
	a := newTestAccount("a", 100, true)
	tripped := false
	for i := 0; i < 3; i++ {
		if a.NoteError(3, 60, 600) {
			tripped = true
		}
	}
	if !tripped {
		t.Fatal("连败 3 次应熔断")
	}
	if !a.InBreaker() {
		t.Error("熔断状态应生效")
	}
	if a.Healthy() {
		t.Error("熔断中的账号不应健康")
	}
	a.NoteSuccess()
	if a.InBreaker() {
		t.Error("成功后应解除熔断")
	}
}

// TestFloorBlocksLowBalance 积分保底生效。
func TestFloorBlocksLowBalance(t *testing.T) {
	p := New("")
	p.SetOptions(Options{BalanceFloor: 100, PickTopN: 5, Strategy: "weighted",
		SoftRateBase: 600, SoftRateMax: 7200, NotFoundCool: 60, CooldownFallback: 300,
		BreakerThreshold: 5, BreakerBase: 60, BreakerMax: 1800,
		StickyTTL: 1800, StickyMaxEntries: 100})
	p.Add(newTestAccount("low", 50, true))
	if _, err := p.Acquire(nil, "m", ""); err == nil {
		t.Fatal("低于保底的账号不应接单")
	}
	p.Add(newTestAccount("rich", 500, true))
	acc, err := p.Acquire(nil, "m", "")
	if err != nil {
		t.Fatalf("高余额账号应能接单：%v", err)
	}
	if acc.Name != "rich" {
		t.Errorf("选中 %s，期望 rich", acc.Name)
	}
}

// TestOverviewCounts 汇总计数正确。
func TestOverviewCounts(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 200, true))
	p.Add(newTestAccount("c", 50, false))
	p.Get("b").Cool(300, CoolSoft, "test")

	ov := p.Overview()
	if ov["total"] != 3 {
		t.Errorf("total = %v，期望 3", ov["total"])
	}
	if ov["healthy"] != 1 {
		t.Errorf("healthy = %v，期望 1", ov["healthy"])
	}
	if ov["cooling"] != 1 {
		t.Errorf("cooling = %v，期望 1", ov["cooling"])
	}
	if ov["disabled"] != 1 {
		t.Errorf("disabled = %v，期望 1", ov["disabled"])
	}
}

// TestClassifyQuota 额度类错误识别为硬冷却。
func TestClassifyQuota(t *testing.T) {
	cases := []struct {
		status int
		body   string
	}{
		{402, `{"error":"payment required"}`},
		{403, `{"error":"积分不足，无法继续"}`},
		{429, `{"error":"insufficient balance"}`},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != FaultQuota {
			t.Errorf("Classify(%d, %q) = %v，期望 FaultQuota", c.status, c.body, got)
		}
	}
}

// TestClassifySession 登录态失效识别。
func TestClassifySession(t *testing.T) {
	if got := Classify(401, `{"error":"unauthorized"}`); got != FaultSession {
		t.Errorf("401 应为 FaultSession，实际 %v", got)
	}
	if got := Classify(400, `{"error":"session not found"}`); got != FaultSession {
		t.Errorf("session not found 应为 FaultSession，实际 %v", got)
	}
}

// TestClassifyModelLimit 模型级限流与账号级频控要能区分。
func TestClassifyModelLimit(t *testing.T) {
	if got := Classify(429, `{"error":"model rate limit exceeded"}`); got != FaultModelLimit {
		t.Errorf("模型级限流应识别为 FaultModelLimit，实际 %v", got)
	}
	if got := Classify(429, `{"error":"too many requests"}`); got != FaultRateLimit {
		t.Errorf("账号级频控应为 FaultRateLimit，实际 %v", got)
	}
}

// TestClassifyServerAndClient 5xx 喂熔断、4xx 不罚账号。
func TestClassifyServerAndClient(t *testing.T) {
	if got := Classify(502, "bad gateway"); got != FaultServer {
		t.Errorf("502 应为 FaultServer，实际 %v", got)
	}
	if got := Classify(400, `{"error":"messages 不能为空"}`); got != FaultClient {
		t.Errorf("400 应为 FaultClient，实际 %v", got)
	}
	if !FatalRequest(FaultClient) {
		t.Error("客户端错误不应换号重试")
	}
	if FatalRequest(FaultQuota) {
		t.Error("额度错误应该换号重试")
	}
}

// TestNextDailyResetInFuture 硬冷却截止时间必须在未来。
func TestNextDailyResetInFuture(t *testing.T) {
	got := NextDailyReset()
	if got <= nowF() {
		t.Errorf("下次重置 %.0f 不在未来（now %.0f）", got, nowF())
	}
	// 不超过 25 小时
	if got-nowF() > 25*3600 {
		t.Errorf("重置时间过远：%.1f 小时后", (got-nowF())/3600)
	}
}

// TestStickySessionKeyUsesLastUser 会话键取最后一条 user（避免多账号退化）。
func TestStickySessionKeyUsesLastUser(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "第一条"},
			map[string]any{"role": "assistant", "content": "回复"},
			map[string]any{"role": "user", "content": "第二条"},
		},
	}
	k1 := SessionKey(payload)
	if k1 == "" {
		t.Fatal("应派生出会话键")
	}
	// 多轮对话换了最后一条 → 键必须变（否则粘性永久锁死同一账号）
	payload["messages"] = append(payload["messages"].([]any),
		map[string]any{"role": "assistant", "content": "再回复"},
		map[string]any{"role": "user", "content": "第三条"},
	)
	k2 := SessionKey(payload)
	if k1 == k2 {
		t.Error("最后一条 user 变化后会话键应变化")
	}
	// 同一条消息重试 → 键不变（粘性只对重试生效）
	if SessionKey(payload) != k2 {
		t.Error("同内容应得到稳定键")
	}
}

// TestStickySessionKeyExplicitID 显式 conversation_id 优先。
func TestStickySessionKeyExplicitID(t *testing.T) {
	payload := map[string]any{
		"conversation_id": "abc",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if got := SessionKey(payload); got != "c-abc" {
		t.Errorf("显式 ID 应优先，得到 %q", got)
	}
}

// TestStickyTTLExpiry TTL 到期后绑定失效。
func TestStickyTTLExpiry(t *testing.T) {
	s := NewSticky(0.05, 10)
	s.Bind("k", "acc")
	if s.Get("k") != "acc" {
		t.Fatal("刚绑定的应命中")
	}
	time.Sleep(80 * time.Millisecond)
	if s.Get("k") != "" {
		t.Error("过期后不应命中")
	}
}

// TestStickyCapacityEviction 容量满时淘汰最早到期者，新会话仍能绑上。
func TestStickyCapacityEviction(t *testing.T) {
	s := NewSticky(1800, 3)
	for i := 0; i < 10; i++ {
		s.Bind(string(rune('a'+i)), "acc")
	}
	snap := s.Snapshot()
	if snap["entries"].(int) > 3 {
		t.Errorf("条目数 %v 超过容量 3", snap["entries"])
	}
	s.Bind("newest", "acc")
	if s.Get("newest") != "acc" {
		t.Error("新键应能绑上")
	}
}

// TestStickyDisabled ttl=0 时粘性关闭。
func TestStickyDisabled(t *testing.T) {
	s := NewSticky(0, 100)
	s.Bind("k", "acc")
	if s.Get("k") != "" {
		t.Error("ttl=0 时不应命中")
	}
	if s.Enabled() {
		t.Error("ttl=0 时应判定为未启用")
	}
}

// TestPoolStatePersistence 计数与开关能落盘并恢复。
func TestPoolStatePersistence(t *testing.T) {
	dir := t.TempDir()
	stateFile := dir + "/state.json"

	p := New(stateFile)
	p.Add(newTestAccount("a", 100, true))
	p.Get("a").Requests = 42
	p.Get("a").PointsUsed = 1234
	p.Get("a").Failures = 3
	p.Get("a").Enabled = false
	p.Flush()
	p.Close()

	// 新池：先加账号再合并状态
	p2 := New(stateFile)
	defer p2.Close()
	p2.Add(newTestAccount("a", 100, true))
	p2.ApplyState()

	got := p2.Get("a")
	if got.Requests != 42 {
		t.Errorf("requests = %d，期望 42", got.Requests)
	}
	if got.PointsUsed != 1234 {
		t.Errorf("pointsUsed = %d，期望 1234", got.PointsUsed)
	}
	if got.Failures != 3 {
		t.Errorf("failures = %d，期望 3", got.Failures)
	}
	if got.Enabled {
		t.Error("enabled=false 应被恢复")
	}
}

// TestPoolRemove 移除账号后池与列表同步。
func TestPoolRemove(t *testing.T) {
	p := New("")
	p.Add(newTestAccount("a", 100, true))
	p.Add(newTestAccount("b", 100, true))
	p.Remove("a")
	if p.Count() != 1 {
		t.Errorf("移除后数量 %d，期望 1", p.Count())
	}
	if p.Get("a") != nil {
		t.Error("a 应已被移除")
	}
	if p.Get("b") == nil {
		t.Error("b 不应受影响")
	}
}

// TestPoolAddReplacesCredentialPreservingCounters 同名再添加应保留计数。
func TestPoolAddReplacesCredentialPreservingCounters(t *testing.T) {
	p := New("")
	old := newTestAccount("a", 100, true)
	old.Requests = 7
	p.Add(old)

	fresh := &Account{Name: "a", Enabled: true, Session: "new-session",
		ExpireAt: time.Now().Add(time.Hour).Unix()}
	p.Add(fresh)

	got := p.Get("a")
	if got.Session != "new-session" {
		t.Errorf("session 应被更新，实际 %q", got.Session)
	}
	if got.Requests != 7 {
		t.Errorf("计数应保留 7，实际 %d", got.Requests)
	}
}

// TestClassifySessionBodyOnly 上游给 200+错误信封（已改判 502）时，文案里的
// 「登录已失效」仍要判成 FaultSession，才能触发重登而不是当服务器故障。
func TestClassifySessionBodyOnly(t *testing.T) {
	body := `{"code":"100002","desc":"登录已失效，请重新登录","data":{}}`
	if got := Classify(502, body); got != FaultSession {
		t.Errorf("登录已失效应为 FaultSession，实际 %v", got)
	}
}

// TestQuotaSurvivesRestart 额度快照必须落盘：面板重启后积分/签到状态不能归零。
func TestQuotaSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	avail, perm, daily, quota, used := int64(10000), int64(5000), int64(5000), int64(5000), int64(0)
	a := &Account{
		Name: "acc1", LoginID: "13800000000", Enabled: true, Session: "s1",
		ExpireAt:  time.Now().Add(72 * time.Hour).Unix(),
		Available: &avail, Permanent: &perm, DailyBalance: &daily,
		DailyQuota: &quota, DailyUsed: &used, DailyCycleDate: "2026-10-21", QuotaAt: 1792390000,
	}
	if err := SaveAccount(dir, a); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	loaded, err := LoadAccounts(dir)
	if err != nil {
		t.Fatalf("LoadAccounts: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("账号数 %d", len(loaded))
	}
	got := loaded[0]
	if got.Available == nil || *got.Available != 10000 {
		t.Errorf("available 未恢复: %v", got.Available)
	}
	if got.Permanent == nil || *got.Permanent != 5000 {
		t.Errorf("permanent 未恢复: %v", got.Permanent)
	}
	if got.DailyQuota == nil || *got.DailyQuota != 5000 {
		t.Errorf("daily_quota 未恢复: %v", got.DailyQuota)
	}
	if got.DailyCycleDate != "2026-10-21" {
		t.Errorf("daily_cycle_date 未恢复: %q", got.DailyCycleDate)
	}
	if got.QuotaAt != 1792390000 {
		t.Errorf("quota_updated_at 未恢复: %d", got.QuotaAt)
	}
}

// TestRemarkSurvivesRestart 备注属于账号资料：落盘 → 重新加载必须原样回来。
// 它与额度快照是同一条约束——persisted() 是「整体覆盖」，漏字段等于静默清零。
func TestRemarkSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	a := &Account{
		Name: "acc1", Enabled: true, Session: "s1",
		Remark: "主力号（联通）",
	}
	if err := SaveAccount(dir, a); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	loaded, err := LoadAccounts(dir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("LoadAccounts: %v / %d", err, len(loaded))
	}
	if loaded[0].Remark != "主力号（联通）" {
		t.Errorf("remark 未恢复: %q", loaded[0].Remark)
	}

	// 再存一次不带备注的（模拟清除）：文件里不能留下空键。
	loaded[0].Remark = ""
	if err := SaveAccount(dir, loaded[0]); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "acc1.json"))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if strings.Contains(string(raw), `"remark"`) {
		t.Errorf("清除后不应留下空 remark 键: %s", raw)
	}
}

// TestDailyAccountingSurvivesRestart 当日记账落盘并在重启后回读（否则面板上的
// 「今日赠送剩余 / 今日永久消耗」每次重启都从零起算，「今日」名不副实）。
func TestDailyAccountingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	daily := int64(5000)
	a := &Account{Name: "a", Enabled: true, Session: "s",
		DailyBalance: &daily, DailyCycleDate: todayKey()}
	a.Charge(30)

	p := New(state)
	p.Add(a)
	p.Flush()

	// 重启：新池从同一个 state.json 恢复
	p2 := New(state)
	p2.Add(&Account{Name: "a", Enabled: true, Session: "s",
		DailyBalance: &daily, DailyCycleDate: todayKey()})
	p2.ApplyState()
	back := p2.Get("a")
	if back.DailyConsumed() != 30 {
		t.Errorf("重启后当日消耗应恢复为 30，实际 %d", back.DailyConsumed())
	}

	// 跨日的旧账不该被当成今天
	a2 := &Account{Name: "b", Enabled: true, Session: "s"}
	p3 := New(state)
	p3.Add(a2)
	p3.pendingState = map[string]map[string]any{
		"b": {"dayKey": "2020-01-01", "dailyConsumed": float64(999), "permBurned": float64(888)},
	}
	p3.ApplyState()
	if a2.DailyConsumed() != 0 || a2.PermBurnedToday() != 0 {
		t.Errorf("昨天的账不该读成今天：%d / %d", a2.DailyConsumed(), a2.PermBurnedToday())
	}
}
