// invites.go 邀请码：查看（自己的码 + 使用数量）、激活状态扫描、绑定、兑换码。
//
// 上游只给三样东西（2026-11-13 用真实号核对过）：
//   - first-login 的 invitationCodes：这个号**自己的**码 + usedCount / maxUses / status；
//   - GET  /points/activation：是否已激活 + 已绑定的那个码；
//   - POST /points/activation：绑定（**一次性、不可逆**，已激活的号再绑没有收益）。
//     没有"谁用了我的码"的明细接口（points/records 的 record_type 被上游忽略）。
//
// 所以这里的设计原则是：读的部分随便查、写的部分必须先看得见状态。
// 绑定只对「已确认未激活」的号执行，状态未知的一律拒绝 —— 一次误绑没法回滚。
package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/loomy2api-panel/internal/pool"
	"github.com/misakano7545/loomy2api-panel/internal/upstream"
)

// inviteAccounts 邀请页数据（本地口径，不打上游）。
func (p *Panel) inviteAccounts(w http.ResponseWriter, r *http.Request) {
	rows := []map[string]any{}
	var codes, used, slots, activated, unactivated, unknown int
	for _, a := range p.pool.All() {
		inv := a.Invite()
		act := a.IsActivated()
		if inv.Code != "" {
			codes++
			used += int(inv.Used)
			if inv.Max > inv.Used {
				slots += int(inv.Max - inv.Used)
			}
		}
		switch {
		case act == nil:
			unknown++
		case *act:
			activated++
		default:
			unactivated++
		}
		rows = append(rows, map[string]any{
			"name":        a.Name,
			"remark":      a.Remark,
			"enabled":     a.Enabled,
			"has_session": a.HasSession(),
			// 面板自有字段名（code/used/max/status）：上游那套 inviteCode/usedCount
			// 只该留在 upstream 包里，别让前端跟着上游命名走。
			"invite": map[string]any{
				"code": inv.Code, "status": inv.Status, "used": inv.Used, "max": inv.Max,
			},
			"activated":      act, // null = 还没扫描过
			"applied_invite": a.AppliedInvite,
			"activation_at":  a.ActivationAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"summary": map[string]any{
			"accounts":     len(rows),
			"with_code":    codes,
			"used_total":   used,
			"slots_left":   slots,
			"activated":    activated,
			"unactivated":  unactivated,
			"unknown":      unknown, // 状态未知：需先「扫描激活状态」
			"max_uses_sum": codes * 20,
		},
		"accounts":                rows,
		"new_account_invite_code": p.cfg.NewAccountInviteCode,
	})
}

// inviteScan POST：批量查激活状态（只读）+ 补齐还没拿到的邀请码。
//
// 邀请码与激活状态来自两个不同端点：码只有 first-login 给（所以顺带刷一次额度，
// 顺带领当日赠送），激活状态走 GET /points/activation。两件事都只读，做成显式按钮，
// 结果落盘 —— 激活状态是一次性事实、邀请码会随使用数变化，扫过之后本地就有数了。
//
// 已经有码的号不再重复刷额度（省上游调用）：第二次点扫描只查激活状态。
func (p *Panel) inviteScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var okN, failN, actN int
	failures := []map[string]any{}

	for _, a := range p.pool.All() {
		if !a.Enabled || !a.HasSession() {
			continue
		}
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// 还没有码的号：顺带刷一次额度（内含 first-login），把邀请码与
			// 使用数量一起拿回来 —— 否则邀请页上「自己的码」要等 30 分钟的
			// 常规额度刷新才出现。
			if a.Invite().Code == "" {
				p.refreshQuota(a)
			}
			act, err := p.upstream.Activation(ctx, a.Session)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failN++
				if len(failures) < 10 {
					failures = append(failures, map[string]any{"name": a.Name, "error": err.Error()})
				}
				return
			}
			a.SetActivation(act)
			okN++
			if act.Activated {
				actN++
			}
			p.persist(a)
		}(a)
	}
	wg.Wait()
	p.Logf("邀请：扫描激活状态 —— 成功 %d / 失败 %d，其中已激活 %d", okN, failN, actN)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "scanned": okN, "failed": failN,
		"activated": actN, "errors": failures,
	})
}

// inviteBind POST：绑定邀请码。默认只列名单（dry_run），真要写得显式关掉。
//
// body: {"code":"XXXXXX", "account":"可选，只绑这一个", "dry_run":true}
//
// 安全约束（绑定不可逆，误绑无法回滚）：
//   - 只绑「已确认未激活」的号（Activated == false）；
//   - 状态未知（没扫描过）的号一律跳过并在响应里点名，让操作者先扫描；
//   - 单号模式也要过这条检查。
func (p *Panel) inviteBind(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code    string `json:"code"`
		Account string `json:"account"`
		DryRun  *bool  `json:"dry_run"`
	}
	// 空请求体按「全默认」处理（dry_run 默认 true），所以 EOF 不算错。
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "请输入邀请码")
		return
	}
	dry := body.DryRun == nil || *body.DryRun // 默认只预演

	type target struct {
		acc    *pool.Account
		reason string
	}
	// 三个都初始化为空切片而不是 nil：JSON 里 null 与 [] 对调用方差别很大
	// （单号模式下 skipped 本来是 nil，前端与脚本都要额外判空）。
	ready := []*pool.Account{}
	skipped := []map[string]any{}
	all := p.pool.All()
	if body.Account != "" {
		a := p.pool.Get(strings.TrimSpace(body.Account))
		if a == nil {
			writeErr(w, http.StatusNotFound, "账号不存在: "+body.Account)
			return
		}
		all = []*pool.Account{a}
	} else {
		all = append([]*pool.Account{}, all...)
	}
	for _, a := range all {
		if !a.Enabled || !a.HasSession() {
			skipped = append(skipped, map[string]any{"name": a.Name, "reason": "禁用或无登录态"})
			continue
		}
		act := a.IsActivated()
		switch {
		case act == nil:
			skipped = append(skipped, map[string]any{"name": a.Name, "reason": "激活状态未知（请先扫描）"})
		case *act:
			skipped = append(skipped, map[string]any{"name": a.Name, "reason": "已激活（绑定是一次性的，再绑无收益）"})
		default:
			ready = append(ready, a)
		}
	}

	if dry {
		names := make([]string, 0, len(ready))
		for _, a := range ready {
			names = append(names, a.Name)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "dry_run": true, "code": code,
			"will_bind": names, "will_bind_count": len(ready), "skipped": skipped,
			"note": "预演：未写入任何东西。确认后带 dry_run=false 再调一次。",
		})
		return
	}
	if len(ready) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "dry_run": false, "code": code, "bound": 0,
			"skipped": skipped, "results": []any{},
			"note": "没有可绑定的号（都已激活或状态未知）",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	var mu sync.Mutex
	var okN, failN int
	results := []map[string]any{}
	for _, a := range ready {
		wg.Add(1)
		go func(a *pool.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			obj, err := p.upstream.BindInvite(ctx, a.Session, code, a.Identity.CampusDev)
			mu.Lock()
			defer mu.Unlock()
			item := map[string]any{"name": a.Name, "ok": err == nil}
			if err != nil {
				item["error"] = err.Error()
				failN++
			} else {
				item["desc"] = sAny(obj["desc"])
				okN++
				// 上游成功即视为已激活：把状态写回，避免下一次又把它算进可绑名单。
				yes := true
				a.SetActivation(&upstream.Activation{Activated: yes, Applied: code})
				p.persist(a)
			}
			results = append(results, item)
		}(a)
	}
	wg.Wait()
	p.Logf("邀请：用码 %s 绑定 —— 成功 %d / 失败 %d（跳过 %d）", code, okN, failN, len(skipped))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "dry_run": false, "code": code, "bound": okN,
		"failed": failN, "skipped": skipped, "results": results,
	})
}

// inviteRedeem POST：兑换积分码（POST /points/redemption-codes/redeem）。
//
// body: {"code":"...", "account":"必须指定"} —— 兑换的分进谁的账，得由操作者点名，
// 不像签到那样可以对全池无脑跑。
func (p *Panel) inviteRedeem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code    string `json:"code"`
		Account string `json:"account"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	code := strings.TrimSpace(body.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "请输入兑换码")
		return
	}
	acc := p.pool.Get(strings.TrimSpace(body.Account))
	if acc == nil {
		writeErr(w, http.StatusBadRequest, "请指定用哪个账号兑换")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	obj, err := p.upstream.Redeem(ctx, acc.Session, code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.Logf("邀请：账号 %s 兑换码 %s —— %s", acc.Name, code, sAny(obj["desc"]))
	// 兑换会改额度，顺手刷新一次快照（失败不影响兑换结果）。
	p.refreshQuota(acc)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": acc.Name, "desc": sAny(obj["desc"]), "data": obj["data"],
	})
}

// claimInviteForNewAccount 给新加的号做一次「首登归因」。
//
// 邀请码只在首次登录/激活那一次生效（激活一次性、不可逆），所以归因必须发生在
// 加号时；对已经有 session 的老号再调是幂等空操作，不会改归属。
func (p *Panel) claimInviteForNewAccount(acc *pool.Account, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := p.upstream.CheckinWithInvite(ctx, acc.Session, code, acc.Identity.CampusDev)
	if err != nil {
		p.Logf("账号 %s 邀请码 %s 归因失败：%v", acc.Name, code, err)
		acc.SetLastError("邀请码归因失败: " + err.Error())
		p.persist(acc)
		return
	}
	acc.SetInvites(res.Invitations)
	if res.Quota != nil {
		acc.SetQuota(res.Quota)
	}
	p.Logf("账号 %s 已用邀请码 %s 首登归因（每日 %d/%d，永久 %d，可用 %d）",
		acc.Name, code, res.DailyQuota-res.DailyConsumed, res.DailyQuota, res.PermanentBalance, res.CurrentBalance)
	p.persist(acc)
}
