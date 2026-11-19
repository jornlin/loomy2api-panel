// probe.go 账号池 · 单账号模型测活。
//
// 为什么不能复用 /v1/chat/completions：那条链路经 Pool.Acquire 选号，测的是
// 「池里随便一个能用的号」，不是用户点开的那个号。测活必须拿**指定账号**自己的
// session 直连上游，才能回答「这个模型在这个号下可用吗」。
//
// 计费与状态：一次测活就是一次真实对话请求，会扣积分。成功照常记台账并
// ReportSuccess（清软冷却 / 登录态存疑）；失败按错误分类处置，唯一例外是 404 ——
// 404 属于模型级问题，但一次手动探测不应该把模型写进全池的「已下架」台账。
//
// 判定口径：
//   - 文本：默认问 2+2，期望正文里出现 4；自定义提问只判断「有没有正文」。
//   - 视觉：注入一张程序生成的三色图形（橙三角 / 紫圆 / 棕方块），要求按从左到右
//     说出颜色顺序；顺序对才算视觉可用。
//   - 上游静默回落（响应 model ≠ 请求 model）直接判 fail：请求写对了名字却执行了
//     别的模型，答案再对也不算这个模型可用，而且是这个网关最会悄悄烧钱的失败。
package panel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/loomy2api-panel/internal/config"
	"github.com/misakano7545/loomy2api-panel/internal/pool"
	"github.com/misakano7545/loomy2api-panel/internal/server"
	"github.com/misakano7545/loomy2api-panel/internal/usage"
)

const (
	probeModeText   = "text"
	probeModeVision = "vision"
	probeModeBoth   = "both"

	// 推理模型会先花 completion token 思考。512 的旧预算在 DeepSeek-V4.1-Flash
	// 上实测会把正文烧成 0 字（思考 512 + 正文 0），测活会被误判成「没正文」。
	probeMaxTokens = 4096
	// 非流式上游对久无响应约 60s 回 504；这里给足长思考时间，但别让面板请求挂死。
	probePerRequestTimeout = 300 * time.Second

	probeTextPrompt   = "请只回复一个数字：2 + 2 等于多少？不要解释。"
	probeTextExpected = "4"
	probeVisionPrompt = "图中从左到右依次排列着三个图形。请只按从左到右的顺序回答每个图形的颜色（中文颜色名），用顿号分隔，不要解释。"
)

// 三个颜色刻意不用红/绿/蓝：看不见图的模型最容易猜的就是三原色，
// 用橙/紫/棕能把「真看见」与「照着常见答案编」区分开。
var probeVisionColors = [][]string{
	{"橙", "橘", "orange"},
	{"紫", "purple"},
	{"棕", "褐", "咖啡", "brown"},
}

// 2+2 的答案不止「4」一种写法：数字必须是独立数字（避免 42 命中），
// 中文「四」与英文 four 也认——否则一个正常模型答「四」会被误判成答案不符。
var probeTextAnswer = regexp.MustCompile(`(^|[^0-9])4([^0-9]|$)`)

func probeTextAnswerOK(content string) bool {
	if probeTextAnswer.MatchString(content) {
		return true
	}
	lower := strings.ToLower(content)
	return strings.Contains(lower, "四") || strings.Contains(lower, "four")
}

// probeCheck 一项测活（文本 / 视觉）的结果。字段直接给前端展示与原始响应核对。
type probeCheck struct {
	Kind          string         `json:"kind"`
	Level         string         `json:"level"` // ok | warn | fail
	OK            bool           `json:"ok"`
	Summary       string         `json:"summary"`
	Notes         []string       `json:"notes,omitempty"`
	Prompt        string         `json:"prompt"`
	Expected      string         `json:"expected,omitempty"`
	Observed      string         `json:"observed,omitempty"`
	HTTPStatus    int            `json:"http_status"`
	UpstreamModel string         `json:"upstream_model,omitempty"`
	Content       string         `json:"content,omitempty"`
	Reasoning     string         `json:"reasoning,omitempty"`
	FinishReason  string         `json:"finish_reason,omitempty"`
	Usage         map[string]any `json:"usage,omitempty"`
	Points        int64          `json:"points"`
	LatencyMs     int64          `json:"latency_ms"`
	RequestID     string         `json:"request_id,omitempty"`
	Error         string         `json:"error,omitempty"`
	Raw           string         `json:"raw,omitempty"`
}

// probeResult 一次按钮点击的完整结果（可能含文本 + 视觉两项）。
type probeResult struct {
	OK              bool          `json:"ok"`
	Account         string        `json:"account"`
	Model           string        `json:"model"`
	Mode            string        `json:"mode"`
	Verdict         string        `json:"verdict"` // ok | warn | fail
	Summary         string        `json:"summary"`
	Checks          []*probeCheck `json:"checks"`
	Image           string        `json:"image,omitempty"` // 视觉测活用的图（data URL），前端直接展示
	Points          int64         `json:"points"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	StartedAt       int64         `json:"started_at"`
	LatencyMs       int64         `json:"latency_ms"`
}

// probeMemo 账号行上的「上次测活」摘要（内存态，重启即忘；完整结果在弹窗里）。
type probeMemo struct {
	Verdict   string `json:"verdict"`
	Summary   string `json:"summary"`
	Model     string `json:"model"`
	Mode      string `json:"mode"`
	At        int64  `json:"at"`
	LatencyMs int64  `json:"latency_ms"`
	Points    int64  `json:"points"`
}

// probeAccount POST /panel/api/accounts/{name}/probe
func (p *Panel) probeAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := p.pool.Get(name)
	if acc == nil {
		writeErr(w, http.StatusNotFound, "账号不存在: "+name)
		return
	}
	if acc.Session == "" {
		writeErr(w, http.StatusBadRequest, "该账号没有登录态，先登录或补凭证")
		return
	}

	req, err := readProbeRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	model := strings.TrimSpace(probeStr(req["model"]))
	if model == "" {
		// 优先取上游目录里的真实 id：配置里的 default_model 可能还是已下架的历史 id。
		if list := p.models.Cached(); len(list) > 0 {
			for _, m := range list {
				if !m.IsImage() {
					model = m.ID
					break
				}
			}
		}
	}
	if model == "" {
		model = p.cfg.DefaultModel
	}
	if model == "" {
		writeErr(w, http.StatusBadRequest, "请指定要测活的模型")
		return
	}

	mode := strings.ToLower(strings.TrimSpace(probeStr(req["mode"])))
	if mode == "" {
		mode = probeModeText
	}
	if mode == "all" {
		mode = probeModeBoth
	}
	switch mode {
	case probeModeText, probeModeVision, probeModeBoth:
	default:
		writeErr(w, http.StatusBadRequest, "mode 只支持 text / vision / both")
		return
	}

	info, known := p.probeModel(model)
	if known && info.IsImage() {
		writeErr(w, http.StatusBadRequest,
			"模型 "+info.ID+" 是图像生成模型，不能走对话测活（应使用 /v1/images/generations）")
		return
	}

	// 思考档位：测活默认关掉（none），把积分和等待时间花在「能不能答」而不是
	// 「想了多久」上。模型不支持 none 时前端不传，走上游默认。
	effort := strings.ToLower(strings.TrimSpace(probeStr(req["reasoning_effort"])))
	if effort != "" && !probeEffortAllowed(info, known, effort) {
		allowed := "none/low/medium/high/xhigh"
		if known && len(info.ReasoningTiers) > 0 {
			allowed = strings.Join(info.ReasoningTiers, "/")
		}
		writeErr(w, http.StatusBadRequest, "该模型不支持思考档位 "+effort+"（可选："+allowed+"）")
		return
	}

	prompt := strings.TrimSpace(probeStr(req["prompt"]))
	if n := len([]rune(prompt)); n > 4000 {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("自定义提问过长（%d 字，上限 4000）", n))
		return
	}

	// 自定义视觉图：上游对 data URI 的支持并不一致（实测 DeepSeek / GLM / qwen
	// 都会把 base64 当成文本、回「看不到图」），只接受公网图片 URL 的上游需要这个入口。
	customImage := strings.TrimSpace(probeStr(req["image_url"]))
	if customImage != "" && mode != probeModeText {
		lower := strings.ToLower(customImage)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			writeErr(w, http.StatusBadRequest, "image_url 只支持 http(s) 图片地址")
			return
		}
		if len(customImage) > 4096 {
			writeErr(w, http.StatusBadRequest, "image_url 过长（上限 4096 字符）")
			return
		}
	}

	if !p.tryLock(name) {
		writeErr(w, http.StatusConflict, "该账号正在执行其它运维操作（登录 / 签到 / 刷新 / 另一次测活），请稍后重试")
		return
	}
	defer p.unlock(name)

	ctx, cancel := context.WithTimeout(r.Context(), probePerRequestTimeout*2+30*time.Second)
	defer cancel()

	startedAt := time.Now()
	res := &probeResult{
		OK: true, Account: name, Model: model, Mode: mode, ReasoningEffort: effort,
		StartedAt: startedAt.Unix(),
	}

	textPrompt := prompt
	expected := ""
	if textPrompt == "" {
		textPrompt = probeTextPrompt
		expected = probeTextExpected
	} else {
		expected = "" // 自定义提问没有标准答案，只验「有没有正文」
	}

	if mode == probeModeText || mode == probeModeBoth {
		chk := p.runProbe(ctx, acc, model, effort, "text", textPrompt, "")
		chk.Expected = expected
		judgeText(chk)
		if s := probeReadinessNote(info, known, len(p.models.Cached()) > 0); s != "" {
			chk.Notes = append(chk.Notes, s)
		}
		res.Checks = append(res.Checks, chk)
		if stopAfterProbe(chk) {
			goto done
		}
	}
	if mode == probeModeVision || mode == probeModeBoth {
		img, builtin := customImage, false
		if img == "" {
			img, builtin = probeImageDataURL(), true
		}
		res.Image = img
		chk := p.runProbe(ctx, acc, model, effort, "vision", probeVisionPrompt, img)
		judgeVision(chk, builtin)
		if s := probeReadinessNote(info, known, len(p.models.Cached()) > 0); s != "" {
			chk.Notes = append(chk.Notes, s)
		}
		if known && !hasCapability(info, "vision") {
			chk.Notes = append(chk.Notes, "模型目录未标注 vision 能力，视觉请求仅验证实际行为")
		}
		res.Checks = append(res.Checks, chk)
	}

done:
	res.Verdict, res.Summary = verdictOf(res.Checks)
	for _, c := range res.Checks {
		res.Points += c.Points
	}
	res.LatencyMs = time.Since(startedAt).Milliseconds()
	p.rememberProbe(name, probeMemo{
		Verdict: res.Verdict, Summary: res.Summary, Model: res.Model, Mode: res.Mode,
		At: startedAt.Unix(), LatencyMs: res.LatencyMs, Points: res.Points,
	})
	p.Logf("测活 | %s | %s | %s | %s | %s | %dms | %d 分",
		name, model, mode, res.Verdict, res.Summary, res.LatencyMs, res.Points)
	writeJSON(w, http.StatusOK, res)
}

// stopAfterProbe 登录态 / 额度 / 频控类失败时，第二个请求必然同样失败，
// 没必要再花一次积分。模型名错误（404）与 400 也直接停，避免重复打上游。
func stopAfterProbe(c *probeCheck) bool {
	if c.HTTPStatus == 0 || c.HTTPStatus == http.StatusOK {
		return false
	}
	return true
}

// runProbe 向指定账号发一次真实的流式对话请求（读 SSE 后聚合）。
func (p *Panel) runProbe(ctx context.Context, acc *pool.Account, model, effort, kind, prompt, imageURL string) *probeCheck {
	chk := &probeCheck{Kind: kind, Prompt: prompt, Level: "fail"}

	var content any = prompt
	if imageURL != "" {
		content = []any{
			map[string]any{"type": "text", "text": prompt},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
		}
	}
	payload := map[string]any{
		"model":       model,
		"messages":    []any{map[string]any{"role": "user", "content": content}},
		"max_tokens":  probeMaxTokens,
		"temperature": 0,
		// 必须走流式：上游对久无响应的非流式请求约 60s 就回 504（视觉 + 推理实测
		// 150s 收到 openresty 504），而同参数的流式可以跑几分钟。
		"stream": true,
	}
	if effort != "" {
		payload["reasoning_effort"] = effort
	}
	// 与对话热路径共用同一套「按模型改写图片形态」逻辑：默认透传标准 image_url，
	// 只有配置了 image_tag_models 白名单的模型才转 <image_base64> 标记。
	if imageURL != "" {
		server.RewriteImageTags(payload, model, p.cfg.ImageTagModels)
	}

	cctx, cancel := context.WithTimeout(ctx, probePerRequestTimeout)
	defer cancel()
	started := time.Now()
	resp, err := p.upstream.Do(cctx, http.MethodPost, "chat/completions", acc.Session, payload, nil)
	if err != nil {
		chk.LatencyMs = time.Since(started).Milliseconds()
		chk.Summary = "连接异常：" + err.Error()
		if cctx.Err() == context.DeadlineExceeded {
			chk.Summary = fmt.Sprintf("请求超时（%s）", probePerRequestTimeout)
		}
		chk.Error = err.Error()
		p.recordProbeUsage(acc, model, chk, 0, chk.Error)
		return chk
	}

	chk.HTTPStatus = resp.Status
	chk.RequestID = responseRequestID(resp.Header)
	if resp.Status != http.StatusOK {
		body := resp.BodyText()
		chk.LatencyMs = time.Since(started).Milliseconds()
		chk.Raw = clipRunes(body, 8000)
		msg := pool.FaultMessage(body)
		chk.Summary = fmt.Sprintf("上游 HTTP %d：%s", resp.Status, msg)
		chk.Error = clipRunes(body, 2000)
		p.recordProbeUsage(acc, model, chk, resp.Status, msg)
		p.handleProbeFailure(acc, model, resp.Status, body)
		return chk
	}

	// 上游 200 → 读 SSE 增量并聚合成一条完整响应。
	agg := server.NewAggregator()
	sc := server.NewSSEStream(resp.Body)
	frames := 0
	streamErr := ""
	for {
		frame, ok, serr := sc.Next()
		if serr != nil {
			streamErr = serr.Error()
			break
		}
		if !ok {
			break
		}
		frames++
		agg.Feed(frame)
	}
	_ = resp.Body.Close()
	chk.LatencyMs = time.Since(started).Milliseconds()

	obj := agg.Result(model)
	if raw, mErr := json.Marshal(obj); mErr == nil {
		chk.Raw = clipRunes(string(raw), 8000)
	}
	if streamErr != "" {
		chk.Notes = append(chk.Notes, "流式读取中断："+clipRunes(streamErr, 160))
	}
	if frames == 0 {
		chk.Notes = append(chk.Notes, "上游没有返回任何 SSE 帧")
	}

	chk.UpstreamModel = probeStr(obj["model"])
	if chk.RequestID == "" {
		chk.RequestID = probeStr(obj["id"])
	}
	usageObj, _ := obj["usage"].(map[string]any)
	chk.Usage = usageObj
	chk.Points = probeUsagePoints(usageObj)

	chk.Content, chk.Reasoning, chk.FinishReason = probeChoiceParts(obj)
	if chk.UpstreamModel != "" && !sameProbeModel(chk.UpstreamModel, model) {
		chk.Notes = append(chk.Notes,
			"上游静默回落：请求 "+model+"，实际执行 "+chk.UpstreamModel+"（按后者扣分）")
	}
	if strings.EqualFold(chk.FinishReason, "length") {
		chk.Notes = append(chk.Notes, "输出被 max_tokens 截断（finish_reason=length）")
	}

	// 200 = 账号这一侧确实把请求送通了：记成功、清软冷却 / 存疑，并按本地口径记账。
	p.pool.ReportSuccess(acc, "", model, chk.Points)
	p.recordProbeUsage(acc, model, chk, http.StatusOK, "")
	return chk
}

// handleProbeFailure 只有「账号自身的问题」才回写池状态。
//
// 404（模型不存在）不回写：ReportFailure 会把它记进全池的模型下架台账，
// 一次手工探测不该产生这种全局副作用。400 同理，是请求形态问题。
func (p *Panel) handleProbeFailure(acc *pool.Account, model string, status int, body string) {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusTooManyRequests:
		p.pool.ReportFailure(acc, status, body, "", model)
	default:
		if status >= 500 {
			p.pool.ReportFailure(acc, status, body, "", model)
		}
	}
}

func (p *Panel) recordProbeUsage(acc *pool.Account, model string, chk *probeCheck, status int, note string) {
	if p.ledger == nil {
		return
	}
	prompt, completion, reasoning := probeUsageTokens(chk.Usage)
	p.ledger.Record(usage.Record{
		Account: acc.Name, Model: model, Kind: "probe", Status: status,
		Prompt: prompt, Completion: completion, Reasoning: reasoning,
		Points: chk.Points, LatencyMs: chk.LatencyMs, RequestID: chk.RequestID,
		Error: clipRunes(note, 300),
	})
}

// judgeText 文本判定：自定义提问只验正文；默认提问验答案 4。
func judgeText(c *probeCheck) {
	if c.Error != "" && c.HTTPStatus != http.StatusOK {
		c.fail()
		return
	}
	content := strings.TrimSpace(c.Content)
	c.Observed = clipRunes(content, 200)
	if content == "" {
		switch {
		case strings.TrimSpace(c.Reasoning) != "" && strings.EqualFold(c.FinishReason, "length"):
			// 预算被思考吃光：是测活预算问题，不足以判模型死。
			c.Level = "warn"
			c.OK = false
			c.Summary = "只产出思考、没有正文（max_tokens 被思考用尽）；提高预算或关闭思考后重试"
		case strings.TrimSpace(c.Reasoning) != "":
			c.Summary = "上游 200，但只产出思考、没有正文（可判断为不可用）"
			c.fail()
		default:
			c.Summary = "上游 200，但正文为空"
			c.fail()
		}
		if hasProbeNote(c, "上游静默回落") {
			c.Level = "fail"
			c.OK = false
			c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
		}
		return
	}
	if c.Expected == "" {
		c.pass("已返回正文（自定义提问，不做答案校验）")
	} else if probeTextAnswerOK(content) {
		c.pass("回答正确：正文包含 " + c.Expected)
	} else {
		c.Level = "warn"
		c.OK = false
		c.Summary = "响应正常，但答案不符（期望 " + c.Expected + "，实际 " + clipRunes(content, 80) + "）"
	}
	// 静默回落是覆盖性结论：请求的模型根本没执行，答案再对也不算这个模型可用。
	if hasProbeNote(c, "上游静默回落") {
		c.Level = "fail"
		c.OK = false
		c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
		return
	}
	// 截断只影响「答案是否完整」，保留上面已经判好的档位（有正文时最多告警）。
	if hasProbeNote(c, "截断") && c.Level == "ok" {
		c.Level = "warn"
		c.OK = false
		c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
	}
}

// judgeVision 视觉判定：正文里按顺序出现 红 → 绿 → 蓝 才算通过。
func judgeVision(c *probeCheck, builtinImage bool) {
	c.Expected = probeVisionExpected()
	if c.Error != "" && c.HTTPStatus != http.StatusOK {
		c.fail()
		return
	}
	content := strings.TrimSpace(c.Content)
	c.Observed = clipRunes(content, 200)
	if content == "" {
		switch {
		case strings.TrimSpace(c.Reasoning) != "" && strings.EqualFold(c.FinishReason, "length"):
			c.Level = "warn"
			c.OK = false
			c.Summary = "视觉请求只产出思考、没有正文（max_tokens 被思考用尽）；提高预算或关闭思考后重试"
		case strings.TrimSpace(c.Reasoning) != "":
			c.Summary = "上游 200，但只产出思考、没有正文，视觉结果不可读"
			c.fail()
		default:
			c.Summary = "上游 200，但正文为空"
			c.fail()
		}
		if hasProbeNote(c, "上游静默回落") {
			c.Level = "fail"
			c.OK = false
			c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
		}
		return
	}
	// 模型明确说「看不到图」时，先别判视觉失败：内置图是 data URI，而上游对
	// data URI 的支持并不一致（实测三个视觉模型都把它当文本）。这属于测试通道
	// 限制，换个公网图片 URL 往往就能过。
	if probeVisionBlind(content) {
		// 内置图是 data URI，看不到图多半是通道限制；若用户已经给了公网 URL
		// 还是看不到，那就是真正的视觉不可用。
		c.Level = "warn"
		if !builtinImage {
			c.Level = "fail"
		}
		c.OK = false
		c.Summary = "视觉请求已返回，但模型表示看不到图"
		if builtinImage {
			c.Summary += "（内置图是 data URI；可在弹窗里填写公网图片 URL 重试）"
		}
		if hasProbeNote(c, "上游静默回落") {
			c.Level = "fail"
			c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
		}
		return
	}

	lower := strings.ToLower(content)
	idx := 0
	found := 0
	for _, names := range probeVisionColors {
		p, n := firstColorIndex(lower[idx:], names)
		if p < 0 {
			break
		}
		idx += p + n
		found++
	}
	switch {
	case found == len(probeVisionColors):
		c.pass("视觉正确：按顺序识别出 " + probeVisionExpected())
	case found == 0:
		c.Level = "fail"
		c.OK = false
		c.Summary = "视觉失败：正文里没有出现期望的颜色（" + probeVisionExpected() + "）"
	default:
		c.Level = "warn"
		c.OK = false
		c.Summary = fmt.Sprintf("视觉结果部分不符：期望 %s（顺序），实际 %s", probeVisionExpected(), clipRunes(content, 100))
	}
	if hasProbeNote(c, "上游静默回落") {
		c.Level = "fail"
		c.OK = false
		c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
		return
	}
	if hasProbeNote(c, "截断") && c.Level == "ok" {
		c.Level = "warn"
		c.OK = false
		c.Summary = strings.Join(c.Notes, "；") + "；" + c.Summary
	}
}

func (c *probeCheck) pass(summary string) {
	c.Level = "ok"
	c.OK = true
	c.Summary = summary
}

func (c *probeCheck) fail() {
	c.Level = "fail"
	c.OK = false
}

// verdictOf 汇总：全过=可用；有失败但仍有通过=部分可用；全失败=不可用。
func verdictOf(checks []*probeCheck) (verdict, summary string) {
	if len(checks) == 0 {
		return "fail", "没有执行任何测活项"
	}
	okN, warnN, failN := 0, 0, 0
	for _, c := range checks {
		switch c.Level {
		case "ok":
			okN++
		case "warn":
			warnN++
		default:
			failN++
		}
	}
	switch {
	case failN == 0 && warnN == 0:
		return "ok", fmt.Sprintf("可用：%d 项测活全部通过", okN)
	case failN == 0:
		if okN == 0 {
			return "warn", fmt.Sprintf("有告警：%d 项测活未通过", warnN)
		}
		return "warn", fmt.Sprintf("基本可用：%d 项通过，%d 项告警", okN, warnN)
	case okN > 0:
		return "warn", fmt.Sprintf("部分可用：%d 项通过，%d 项失败", okN, failN)
	default:
		return "fail", fmt.Sprintf("不可用：%d 项测活全部失败", failN)
	}
}

// probeVisionExpected 期望答案的展示文本（取每个颜色的第一个名字）。
func probeVisionExpected() string {
	names := make([]string, 0, len(probeVisionColors))
	for _, n := range probeVisionColors {
		names = append(names, n[0])
	}
	return strings.Join(names, "、")
}

// firstColorIndex 任意一个颜色别名在文本里的最早位置与长度（中英文都认）。
func firstColorIndex(s string, names []string) (int, int) {
	best, bestLen := -1, 0
	for _, name := range names {
		if name == "" {
			continue
		}
		if i := strings.Index(s, name); i >= 0 && (best < 0 || i < best) {
			best, bestLen = i, len(name)
		}
	}
	return best, bestLen
}

// probeVisionBlind 判定模型是否在明确表示「我看不到图」。
func probeVisionBlind(content string) bool {
	lower := strings.ToLower(content)
	if strings.Contains(lower, "base64") {
		return true
	}
	saysImage := strings.Contains(lower, "图片") || strings.Contains(lower, "图像") || strings.Contains(lower, "图")
	canNot := strings.Contains(lower, "无法") || strings.Contains(lower, "不能") || strings.Contains(lower, "看不到") ||
		strings.Contains(lower, "读不了") || strings.Contains(lower, "文本模型")
	see := strings.Contains(lower, "读取") || strings.Contains(lower, "识别") || strings.Contains(lower, "查看") ||
		strings.Contains(lower, "解码") || strings.Contains(lower, "解析") || strings.Contains(lower, "获取") ||
		strings.Contains(lower, "看不到")
	return saysImage && canNot && see
}

// probeEffortAllowed 优先按上游目录声明校验；目录没有该模型的档位信息时
// 退回 OpenAI 兼容的通用集合，让上游自己判。
func probeEffortAllowed(m config.Model, known bool, effort string) bool {
	if known && len(m.ReasoningTiers) > 0 {
		for _, e := range m.ReasoningTiers {
			if strings.EqualFold(e, effort) {
				return true
			}
		}
		return false
	}
	switch effort {
	case "none", "low", "medium", "high", "xhigh":
		return true
	}
	return false
}

func hasCapability(m config.Model, cap string) bool {
	for _, c := range m.Capabilities {
		if strings.EqualFold(c, cap) {
			return true
		}
	}
	return false
}

func probeReadinessNote(m config.Model, known, catalogLoaded bool) string {
	if !catalogLoaded {
		return ""
	}
	if !known {
		return "模型不在当前上游目录中：上游通常不报 404，而是静默回落到默认模型并照样扣分（看「执行模型」）"
	}
	if !hasCapability(m, "reasoning") && !hasCapability(m, "vision") {
		return "模型目录未标注 reasoning / vision 能力"
	}
	return ""
}

// probeModel 在缓存的模型目录里按 id 查找（imodel/ 前缀与大小写不敏感）。
func (p *Panel) probeModel(model string) (config.Model, bool) {
	id := strings.TrimPrefix(model, "imodel/")
	for _, m := range p.models.Cached() {
		if strings.EqualFold(strings.TrimPrefix(m.ID, "imodel/"), id) {
			return m, true
		}
	}
	return config.Model{}, false
}

func (p *Panel) rememberProbe(account string, memo probeMemo) {
	p.probeMu.Lock()
	p.probes[account] = memo
	p.probeMu.Unlock()
}

func (p *Panel) lastProbe(account string) (probeMemo, bool) {
	p.probeMu.Lock()
	defer p.probeMu.Unlock()
	m, ok := p.probes[account]
	return m, ok
}

func readProbeRequest(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取请求失败: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// ------------------------------------------------------------------ 响应解析

func probeChoiceParts(obj map[string]any) (content, reasoning, finish string) {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return "", "", ""
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return "", "", ""
	}
	finish = probeStr(choice["finish_reason"])
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return "", "", finish
	}
	content = probeContentText(msg["content"])
	reasoning = probeReasoningText(msg)
	if reasoning == "" {
		reasoning = probeStr(choice["reasoning_content"])
	}
	return content, reasoning, finish
}

func probeContentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, raw := range t {
			part, _ := raw.(map[string]any)
			if part == nil {
				continue
			}
			switch strings.ToLower(probeStr(part["type"])) {
			case "text", "output_text", "input_text":
				b.WriteString(probeStr(part["text"]))
			}
		}
		return b.String()
	case map[string]any:
		return probeStr(t["text"])
	}
	return ""
}

func probeReasoningText(msg map[string]any) string {
	for _, k := range []string{"reasoning_content", "reasoning"} {
		switch v := msg[k].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case map[string]any:
			if s := probeStr(v["content"]); strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

func probeUsageTokens(usage map[string]any) (prompt, completion, reasoning int64) {
	if usage == nil {
		return
	}
	prompt = int64(probeNum(usage["prompt_tokens"]))
	completion = int64(probeNum(usage["completion_tokens"]))
	if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoning = int64(probeNum(d["reasoning_tokens"]))
	}
	return
}

func probeUsagePoints(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	for _, k := range []string{"points_consumed", "points", "point"} {
		if v, ok := usage[k]; ok {
			return int64(probeNum(v))
		}
	}
	return 0
}

func responseRequestID(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "X-Request-ID", "Request-Id"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func sameProbeModel(a, b string) bool {
	return normalizeProbeModel(a) == normalizeProbeModel(b)
}

// normalizeProbeModel 比对模型名时去掉 imodel/ 前缀与上游附带的思考档位后缀：
// spark-x2.6 与 spark-x2.6-low 是同一个模型，不该误报成「静默回落」。
var probeModelSuffix = regexp.MustCompile(`-(none|low|medium|high|xhigh)$`)

func normalizeProbeModel(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "imodel/")
	return probeModelSuffix.ReplaceAllString(s, "")
}

func hasProbeNote(c *probeCheck, sub string) bool {
	for _, n := range c.Notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func probeStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func probeNum(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		var f float64
		_, _ = fmt.Sscanf(t, "%f", &f)
		return f
	}
	return 0
}

func clipRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ------------------------------------------------------------------ 视觉测活图

var (
	probeImgOnce sync.Once
	probeImgURL  string
)

// probeImageDataURL 生成固定的三色图形（橙三角 / 紫圆 / 棕方块），PNG data URL。
//
// 不用外部图片文件：视觉测活的可复现性比「好看」重要——同一张图、同一句问题，
// 判定期望固定为「橙、紫、棕」。纯色图形也避免了 OCR / 文字理解把问题复杂化。
func probeImageDataURL() string {
	probeImgOnce.Do(func() {
		const w, h = 480, 160
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		white := color.RGBA{0xff, 0xff, 0xff, 0xff}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetRGBA(x, y, white)
			}
		}
		orange := color.RGBA{0xff, 0x80, 0x00, 0xff}
		purple := color.RGBA{0x80, 0x00, 0xff, 0xff}
		brown := color.RGBA{0x8b, 0x45, 0x13, 0xff}

		// 橙三角：顶点 (80, 30)，底边 y=125，半宽 42
		for y := 30; y <= 125; y++ {
			half := (y - 30) * 42 / 95
			for x := 80 - half; x <= 80+half; x++ {
				img.SetRGBA(x, y, orange)
			}
		}
		// 紫圆：圆心 (240, 78)，半径 47
		for y := 30; y <= 126; y++ {
			dy := y - 78
			for x := 192; x <= 288; x++ {
				dx := x - 240
				if dx*dx+dy*dy <= 47*47 {
					img.SetRGBA(x, y, purple)
				}
			}
		}
		// 棕方块：40×80
		for y := 38; y <= 118; y++ {
			for x := 360; x <= 440; x++ {
				img.SetRGBA(x, y, brown)
			}
		}

		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return
		}
		probeImgURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	})
	return probeImgURL
}
