package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/loomy2api-panel/internal/pool"
	"github.com/misakano7545/loomy2api-panel/internal/upstream"
)

// newProbeFixture 造一个「刚被上游 401」的账号池 + 指向假上游的客户端。
func newProbeFixture(t *testing.T, upstreamURL string) (*pool.Pool, *upstream.Client, *pool.Account, *int) {
	t.Helper()
	p := pool.New("")
	p.SetOptions(pool.Options{SessionProbeSeconds: 120, SessionDropHits: 3})
	saved := 0
	p.SetAccountSaver(func(a *pool.Account) error { saved++; return nil })

	a := &pool.Account{
		Name: "acc1", Enabled: true,
		Session:    "sess-1",
		ObtainedAt: time.Now().Add(-15 * 24 * time.Hour).Unix(),
	}
	p.Add(a)
	// 上游回过 401 → 进入存疑期（等价于 ReportFailure 里发生的事）。
	p.ReportFailure(a, 401, "unauthorized", "", "m")

	up := upstream.NewClient(upstreamURL, "0.9.38", "", 5*time.Second)
	return p, up, a, &saved
}

// TestProbeSuspectSessionDropsDeadCredential 复核也被 401 拒绝 → 判死并落盘。
func TestProbeSuspectSessionDropsDeadCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"100002","desc":"缺少 token"}`)
	}))
	defer srv.Close()

	p, up, a, saved := newProbeFixture(t, srv.URL)
	probeSuspectSessions(p, up)

	if a.HasSession() {
		t.Error("复核被 401 拒绝应清空登录态")
	}
	if !strings.Contains(a.LastError(), "需重新登录") {
		t.Errorf("应给出可操作的原因，实际 %q", a.LastError())
	}
	if a.ObservedLifetime < int64(14*24*3600) {
		t.Errorf("应记下实测寿命，实际 %d", a.ObservedLifetime)
	}
	if *saved == 0 {
		t.Error("判死必须落盘（否则重启又读回死凭证）")
	}
	if a.Healthy() {
		t.Error("判死后不该可用")
	}
}

// TestProbeSuspectSessionKeepsLiveCredential 复核通过 → 回池，当一次抖动。
func TestProbeSuspectSessionKeepsLiveCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":"000000","data":{"balance":100,"dailyBalance":50,"availableBalance":150}}`)
	}))
	defer srv.Close()

	p, up, a, saved := newProbeFixture(t, srv.URL)
	probeSuspectSessions(p, up)

	if !a.HasSession() {
		t.Error("复核通过不该清凭证")
	}
	if a.InSessionSuspect() {
		t.Error("复核通过应撤销存疑")
	}
	if !a.Healthy() {
		t.Error("复核通过后应回到轮换")
	}
	if *saved != 0 {
		t.Error("复核通过不该落盘")
	}
}

// TestProbeSuspectSessionNetworkFailureNoVerdict 复核拿不到结论（网络层失败）时不下判死。
func TestProbeSuspectSessionNetworkFailureNoVerdict(t *testing.T) {
	// 指向一个必然连不上的端口。
	p, up, a, _ := newProbeFixture(t, "http://127.0.0.1:1")
	probeSuspectSessions(p, up)

	if !a.HasSession() {
		t.Error("网络失败不是判死依据")
	}
	if !a.InSessionSuspect() {
		t.Error("应保持存疑，等窗口到期自动回池")
	}
}

// TestUntilNextDay 跨日刷新定时器：必须落在下一个自然日零点之后、且不超过 24 小时
// （每日赠送按自然日清零，新一天的额度要一次 first-login 才到账；固定 30 分钟的
// ticker 会把这一轮拖到 00:29，那之前每个请求都在烧永久积分）。
func TestUntilNextDay(t *testing.T) {
	d := untilNextDay(20 * time.Second)
	if d <= 0 || d > 24*time.Hour+21*time.Second {
		t.Fatalf("距下次自然日应落在 (0, 24h20s]，实际 %v", d)
	}
	fire := time.Now().Add(d)
	if fire.Hour() != 0 || fire.Minute() != 0 {
		t.Errorf("触发点应落在 00:00 之后不久，实际 %s", fire.Format("2006-01-02 15:04:05"))
	}
	if fire.Before(time.Now()) {
		t.Error("触发点必须在未来")
	}
}
