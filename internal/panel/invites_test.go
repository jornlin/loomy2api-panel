// invites_test.go 邀请码页的四个端点：总览（本地）/ 扫描激活（只读）/ 绑定（默认预演）/ 兑换码。
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/misakano7545/loomy2api-panel/internal/pool"
	"github.com/misakano7545/loomy2api-panel/internal/upstream"
)

// inviteUpstream 假上游：GET activation 按 session 返回不同状态，POST activation 记调用。
func inviteUpstream(t *testing.T, activated map[string]bool, binds *int32, bound *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case strings.Contains(r.URL.Path, "points/activation") && r.Method == http.MethodGet:
			on := activated[sess]
			code := ""
			if on {
				code = "OLDONE"
			}
			_, _ = io.WriteString(w, `{"code":"000000","data":{"activated":`+
				map[bool]string{true: "true", false: "false"}[on]+`,"appliedInvitationCode":"`+code+`"}}`)
		case strings.Contains(r.URL.Path, "points/activation") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			*bound = append(*bound, sess+"|"+s(body["inviteCode"])+"|"+s(body["deviceId"]))
			atomic.AddInt32(binds, 1)
			_, _ = io.WriteString(w, `{"code":"000000","desc":"成功","data":{}}`)
		case strings.Contains(r.URL.Path, "redemption-codes/redeem"):
			_, _ = io.WriteString(w, `{"code":"000000","desc":"兑换成功","data":{"points":500}}`)
		default:
			_, _ = io.WriteString(w, `{"code":"000000","data":{}}`)
		}
	}))
}

func s(v any) string { str, _ := v.(string); return str }

// invitePanel 造一个三号池：acc-new（未激活）、acc-old（已激活）、acc-unknown（没扫过）。
func invitePanel(t *testing.T, srvURL string) (*Panel, *pool.Pool) {
	t.Helper()
	pl, p := newTestPanelUpstream(t, srvURL)
	p.Add(&pool.Account{Name: "acc-new", Enabled: true, Session: "sess-new",
		Invites: []upstream.InviteCode{{Code: "NEWCD1", Status: "active", Max: 20}}})
	p.Add(&pool.Account{Name: "acc-old", Enabled: true, Session: "sess-old",
		Invites: []upstream.InviteCode{{Code: "OLDCD2", Status: "active", Used: 7, Max: 20}}})
	p.Add(&pool.Account{Name: "acc-unknown", Enabled: true, Session: "sess-unknown"})
	no := false
	yes := true
	p.Get("acc-new").SetActivation(&upstream.Activation{Activated: no})
	p.Get("acc-old").SetActivation(&upstream.Activation{Activated: yes, Applied: "OLDONE"})
	return pl, p
}

// TestInviteOverview 总览用本地数据：码数、已用、剩余名额、三种激活状态各归各位。
func TestInviteOverview(t *testing.T) {
	pl, _ := invitePanel(t, "https://upstream.invalid")
	req := httptest.NewRequest(http.MethodGet, "/panel/api/invites", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var out struct {
		Summary  map[string]any `json:"summary"`
		Accounts []struct {
			Name      string `json:"name"`
			Activated *bool  `json:"activated"`
			// 面板自有字段名：别写成上游的 inviteCode/usedCount ——
			// 前端读 code/used/max/status，字段名一漂移整列就空。
			Invite struct {
				Code   string `json:"code"`
				Status string `json:"status"`
				Used   int64  `json:"used"`
				Max    int64  `json:"max"`
			} `json:"invite"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	sm := out.Summary
	if sm["accounts"] != float64(4) || sm["with_code"] != float64(2) { // acc1（默认账号）没有码
		t.Errorf("账号/码计数不对: %v", sm)
	}
	if sm["used_total"] != float64(7) {
		t.Errorf("已用合计应为 7，实际 %v", sm["used_total"])
	}
	if sm["slots_left"] != float64(33) { // (20-0)+(20-7)
		t.Errorf("剩余名额应为 33，实际 %v", sm["slots_left"])
	}
	if sm["unactivated"] != float64(1) || sm["activated"] != float64(1) || sm["unknown"] != float64(2) {
		t.Errorf("激活状态分布不对: %v", sm)
	}
	// 行的字段名与数值都要对得上（前端直接吃这几个键）。
	seen := map[string]bool{}
	for _, a := range out.Accounts {
		if a.Name == "acc-old" {
			if a.Invite.Code != "OLDCD2" || a.Invite.Used != 7 || a.Invite.Max != 20 || a.Invite.Status != "active" {
				t.Errorf("acc-old 的邀请码字段不对: %+v", a.Invite)
			}
			if a.Activated == nil || !*a.Activated {
				t.Errorf("acc-old 应记为已激活: %v", a.Activated)
			}
			seen["old"] = true
		}
		if a.Name == "acc-unknown" && a.Activated != nil {
			t.Errorf("没扫过的号必须是 null（前端据此显示「未查询」），实际 %v", *a.Activated)
		}
	}
	if !seen["old"] {
		t.Fatal("总览里没有 acc-old")
	}
}

// TestInviteScan 扫描只读：把每号激活状态写回并落盘，不触发任何绑定。
func TestInviteScan(t *testing.T) {
	var binds int32
	var bound []string
	srv := inviteUpstream(t, map[string]bool{"sess-new": false, "sess-old": true}, &binds, &bound)
	defer srv.Close()
	pl, p := invitePanel(t, srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/panel/api/invites/scan", nil)
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["scanned"] == nil || out["activated"] != float64(1) {
		t.Errorf("扫描结果不对: %v", out)
	}
	if atomic.LoadInt32(&binds) != 0 {
		t.Error("扫描是只读的，不该发生任何绑定")
	}
	if act := p.Get("acc-new").IsActivated(); act == nil || *act {
		t.Errorf("acc-new 应被写成未激活: %v", act)
	}
	if act := p.Get("acc-old").IsActivated(); act == nil || !*act {
		t.Errorf("acc-old 应保持已激活: %v", act)
	}
	if got := p.Get("acc-old").AppliedInvite; got != "OLDONE" {
		t.Errorf("已绑定的码应写回，实际 %q", got)
	}
}

// TestInviteBindDryRunAndGuard 默认预演不写；状态未知的号一律跳过（绑定不可逆）。
func TestInviteBindDryRunAndGuard(t *testing.T) {
	var binds int32
	var bound []string
	srv := inviteUpstream(t, map[string]bool{}, &binds, &bound)
	defer srv.Close()
	pl, _ := invitePanel(t, srv.URL)

	body := `{"code":"NEWCD9"}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/invites/bind", strings.NewReader(body))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var out struct {
		DryRun   bool     `json:"dry_run"`
		WillBind []string `json:"will_bind"`
		Skipped  []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("不是 JSON: %v", err)
	}
	if !out.DryRun {
		t.Error("不传 dry_run 时必须默认预演")
	}
	if len(out.WillBind) != 1 || out.WillBind[0] != "acc-new" {
		t.Errorf("只该列出确认未激活的号，实际 %v", out.WillBind)
	}
	if atomic.LoadInt32(&binds) != 0 {
		t.Error("预演不该真的绑定")
	}
	reasons := map[string]string{}
	for _, s := range out.Skipped {
		reasons[s.Name] = s.Reason
	}
	if !strings.Contains(reasons["acc-unknown"], "未知") {
		t.Errorf("状态未知的号必须被跳过并说明原因，实际 %v", reasons)
	}
	if !strings.Contains(reasons["acc-old"], "已激活") {
		t.Errorf("已激活的号必须被跳过，实际 %v", reasons)
	}
}

// TestInviteBindWrites 真绑：带该号自己的设备号，成功后把状态写成已激活（避免下次重复绑）。
func TestInviteBindWrites(t *testing.T) {
	var binds int32
	var bound []string
	srv := inviteUpstream(t, map[string]bool{}, &binds, &bound)
	defer srv.Close()
	pl, p := invitePanel(t, srv.URL)
	p.Get("acc-new").Identity.CampusDev = "loomy-campus-dev1"

	req := httptest.NewRequest(http.MethodPost, "/panel/api/invites/bind",
		strings.NewReader(`{"code":"NEWCD9","account":"acc-new","dry_run":false}`))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["bound"] != float64(1) {
		t.Fatalf("应绑定 1 个，实际 %v（%s）", out["bound"], w.Body.String())
	}
	if len(bound) != 1 || !strings.HasSuffix(bound[0], "NEWCD9|loomy-campus-dev1") {
		t.Errorf("绑定必须带该号自己的设备号，实际 %v", bound)
	}
	act := p.Get("acc-new").IsActivated()
	if act == nil || !*act {
		t.Errorf("绑定成功后应写入已激活，实际 %v", act)
	}
	if got := p.Get("acc-new").AppliedInvite; got != "NEWCD9" {
		t.Errorf("已绑定的码应写入，实际 %q", got)
	}

	// 再绑一次：已激活 → 不在名单里，不会重复写。
	req = httptest.NewRequest(http.MethodPost, "/panel/api/invites/bind",
		strings.NewReader(`{"code":"OTHER1","account":"acc-new","dry_run":false}`))
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["bound"] != float64(0) {
		t.Errorf("已激活的号不该再绑，实际 bound=%v", out["bound"])
	}
	if atomic.LoadInt32(&binds) != 1 {
		t.Errorf("上游只该被调用 1 次，实际 %d", binds)
	}
}

// TestInviteRedeem 兑换码必须点名账号，并把上游结果原样回报。
func TestInviteRedeem(t *testing.T) {
	var binds int32
	var bound []string
	srv := inviteUpstream(t, map[string]bool{}, &binds, &bound)
	defer srv.Close()
	pl, _ := invitePanel(t, srv.URL)

	req := httptest.NewRequest(http.MethodPost, "/panel/api/invites/redeem",
		strings.NewReader(`{"code":"GIFT-1"}`))
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("不给账号应 400，实际 %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/panel/api/invites/redeem",
		strings.NewReader(`{"code":"GIFT-1","account":"acc-new"}`))
	w = httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["ok"] != true || out["desc"] != "兑换成功" || out["account"] != "acc-new" {
		t.Errorf("兑换结果不对: %v", out)
	}
}
