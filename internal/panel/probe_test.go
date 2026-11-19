package panel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	_ "image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/loomy2api-panel/internal/usage"
)

// probePost 调一次测活接口并解出结果。
func probePost(t *testing.T, pl *Panel, body string) (int, probeResult) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/acc1/probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	pl.ServeHTTP(w, req)
	var res probeResult
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &res)
	}
	return w.Code, res
}

// fakeChat 返回固定 chat 响应；inspect 可检查请求体，返 nil 表示用默认响应。
func fakeChat(t *testing.T, inspect func(body map[string]any), resp map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sess-1" {
			t.Errorf("Authorization = %q，测活必须使用该账号自己的 session", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if inspect != nil {
			inspect(body)
		}
		writeChatSSE(w, resp)
	}))
}

// writeChatSSE 把最终对象写成最小 SSE 流：测活必须走流式，否则上游的
// 非流式长请求会被上游网关 504。聚合器只关心 delta / finish_reason / usage。
func writeChatSSE(w http.ResponseWriter, resp map[string]any) {
	choices, _ := resp["choices"].([]any)
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	delta := map[string]any{"role": "assistant"}
	if s, ok := msg["content"].(string); ok {
		delta["content"] = s
	}
	if s, ok := msg["reasoning_content"].(string); ok {
		delta["reasoning_content"] = s
	}
	frame := map[string]any{
		"id": resp["id"], "model": resp["model"],
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": ch["finish_reason"],
		}},
		"usage": resp["usage"],
	}
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(frame)
	_, _ = w.Write([]byte("data: " + string(b) + "\n\ndata: [DONE]\n\n"))
}

func chatResponse(model, content, reasoning string, points int) map[string]any {
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	usage := map[string]any{"prompt_tokens": 12, "completion_tokens": 5}
	if points > 0 {
		usage["points_consumed"] = points
	}
	return map[string]any{
		"id":    "req-probe-1",
		"model": model,
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       msg,
		}},
		"usage": usage,
	}
}

// TestProbeTextOK 文本测活：200 + 正文 4 = 可用，并写台账 / 记一次成功。
func TestProbeTextOK(t *testing.T) {
	srv := fakeChat(t, func(body map[string]any) {
		if body["model"] != "DeepSeek-V4.1-Flash" {
			t.Errorf("请求模型 = %v", body["model"])
		}
		if max, _ := body["max_tokens"].(float64); int(max) != probeMaxTokens {
			t.Errorf("max_tokens = %v，期望 %d", body["max_tokens"], probeMaxTokens)
		}
		if stream, _ := body["stream"].(bool); !stream {
			t.Error("测活必须走流式（非流式长请求会被上游 504）")
		}
	}, chatResponse("DeepSeek-V4.1-Flash", "4", "", 2))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	code, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text"}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if res.Verdict != "ok" || len(res.Checks) != 1 {
		t.Fatalf("verdict=%s checks=%d", res.Verdict, len(res.Checks))
	}
	c := res.Checks[0]
	if !c.OK || c.Content != "4" || c.Expected != "4" {
		t.Errorf("check = %+v", c)
	}
	if c.Points != 2 || res.Points != 2 {
		t.Errorf("积分 = %d / %d，期望 2", c.Points, res.Points)
	}
	if c.RequestID != "req-probe-1" {
		t.Errorf("request id = %q", c.RequestID)
	}
	acc := p.Get("acc1")
	if acc.Requests != 1 {
		t.Errorf("Requests = %d，成功测活应记一次成功", acc.Requests)
	}
	recs := pl.ledger.Recent(10, usage.RecordFilter{Account: "acc1"})
	if len(recs) != 1 || recs[0].Kind != "probe" || recs[0].Points != 2 {
		t.Errorf("台账 = %+v", recs)
	}
	if memo, ok := pl.lastProbe("acc1"); !ok || memo.Verdict != "ok" {
		t.Errorf("lastProbe = %+v, %v", memo, ok)
	}
}

// TestProbeVisionOK 视觉测活：请求里带 data URI 图，回答红绿蓝 = 可用；返回图可解码。
func TestProbeVisionOK(t *testing.T) {
	srv := fakeChat(t, func(body map[string]any) {
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Errorf("messages = %v", body["messages"])
			return
		}
		msg, _ := msgs[0].(map[string]any)
		parts, _ := msg["content"].([]any)
		found := false
		for _, raw := range parts {
			p, _ := raw.(map[string]any)
			if p != nil && p["type"] == "image_url" {
				u, _ := p["image_url"].(map[string]any)
				if s, _ := u["url"].(string); strings.HasPrefix(s, "data:image/png;base64,") {
					found = true
				}
			}
		}
		if !found {
			t.Error("视觉请求缺少 image_url data URI")
		}
	}, chatResponse("DeepSeek-V4.1-Flash", "橙、紫、棕", "", 3))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	code, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"vision"}`)
	if code != http.StatusOK || res.Verdict != "ok" {
		t.Fatalf("code=%d verdict=%s summary=%s", code, res.Verdict, res.Summary)
	}
	if !strings.HasPrefix(res.Image, "data:image/png;base64,") {
		t.Fatalf("响应没有回传测活图: %q", res.Image)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(res.Image, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 480 || b.Dy() != 160 {
		t.Errorf("测活图尺寸 = %v", b)
	}
	for _, tc := range []struct {
		x, y int
		want color.RGBA
	}{
		{80, 100, color.RGBA{0xff, 0x80, 0x00, 0xff}}, // 橙三角内部
		{240, 78, color.RGBA{0x80, 0x00, 0xff, 0xff}}, // 紫圆圆心
		{400, 78, color.RGBA{0x8b, 0x45, 0x13, 0xff}}, // 棕方块内部
	} {
		got := color.RGBAModel.Convert(img.At(tc.x, tc.y)).(color.RGBA)
		if got.R != tc.want.R || got.G != tc.want.G || got.B != tc.want.B {
			t.Errorf("(%d,%d) = %v，期望 %v", tc.x, tc.y, got, tc.want)
		}
	}
}

// TestProbeSilentFallbackFails 响应 model ≠ 请求 model：请求的模型根本没执行，
// 答案再对也判不可用（这正是测活最该抓的问题）。
func TestProbeSilentFallbackFails(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("DeepSeek-V4.1-Flash", "4", "", 2))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"no-such-model","mode":"text"}`)
	if res.Verdict != "fail" {
		t.Fatalf("verdict = %s，静默回落应为 fail", res.Verdict)
	}
	if !hasProbeNote(res.Checks[0], "静默回落") {
		t.Errorf("notes = %v", res.Checks[0].Notes)
	}
}

// TestProbeModelSuffixNormalize 上游给模型名加 -low 之类的思考档位后缀，
// 不算「换了一个模型」。
func TestProbeModelSuffixNormalize(t *testing.T) {
	if !sameProbeModel("spark-x2.6", "spark-x2.6-low") {
		t.Error("spark-x2.6 与 spark-x2.6-low 应视为同一模型")
	}
	if !sameProbeModel("imodel/DeepSeek-V4.1-Flash", "deepseek-v4.1-flash-xhigh") {
		t.Error("imodel/ 前缀与大小写应归一")
	}
	if sameProbeModel("qwen3.8-flash", "spark-x2.6-low") {
		t.Error("不同模型不应归一")
	}
}

// TestProbeWrongAnswerWarns 响应正常但答案不符：告警而不是直接判死。
func TestProbeWrongAnswerWarns(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("DeepSeek-V4.1-Flash", "5", "", 2))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text"}`)
	if res.Verdict != "warn" || res.Checks[0].Level != "warn" {
		t.Fatalf("verdict=%s level=%s", res.Verdict, res.Checks[0].Level)
	}
}

// TestProbeCustomPromptFallbackFails 自定义提问也不能绕过回落检测：
// 请求的模型没执行，正文再正常也判 fail。
func TestProbeCustomPromptFallbackFails(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("spark-x2.6", "好的，我可以回答。", "", 2))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"qwen3.8-flash","mode":"text","prompt":"请说一句话"}`)
	if res.Verdict != "fail" {
		t.Fatalf("verdict = %s，自定义提问也应检测静默回落", res.Verdict)
	}
}

// TestProbeTextChineseFour 答案写成「四」也算对，避免把正常模型误判成答案不符。
func TestProbeTextChineseFour(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("DeepSeek-V4.1-Flash", "四", "", 2))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text"}`)
	if res.Verdict != "ok" {
		t.Fatalf("verdict = %s，四应视为正确答案", res.Verdict)
	}
}

// TestProbeOnlyReasoningFails 200 但只有思考没有正文：判不可用。
func TestProbeOnlyReasoningFails(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("DeepSeek-V4.1-Flash", "", "想了很多，但正文预算用完了", 2))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text"}`)
	if res.Verdict != "fail" || res.Checks[0].Level != "fail" {
		t.Fatalf("verdict=%s level=%s summary=%s", res.Verdict, res.Checks[0].Level, res.Summary)
	}
}

// TestProbeReasoningBudgetWarns 思考把 max_tokens 吃光、正文为空时，是预算问题，
// 判告警而不是模型不可用。
func TestProbeReasoningBudgetWarns(t *testing.T) {
	resp := chatResponse("DeepSeek-V4.1-Flash", "", "想了很久", 2)
	resp["choices"].([]any)[0].(map[string]any)["finish_reason"] = "length"
	srv := fakeChat(t, nil, resp)
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text"}`)
	if res.Verdict != "warn" || res.Checks[0].Level != "warn" {
		t.Fatalf("verdict=%s level=%s summary=%s", res.Verdict, res.Checks[0].Level, res.Checks[0].Summary)
	}
}

// TestProbeCustomPromptOnlyChecksBody 自定义提问没有标准答案，只验正文非空。
func TestProbeCustomPromptOnlyChecksBody(t *testing.T) {
	srv := fakeChat(t, func(body map[string]any) {
		msgs, _ := body["messages"].([]any)
		msg, _ := msgs[0].(map[string]any)
		if msg["content"] != "用一句话介绍你自己" {
			t.Errorf("自定义提问未透传: %v", msg["content"])
		}
	}, chatResponse("DeepSeek-V4.1-Flash", "我是一个模型。", "", 2))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text","prompt":"用一句话介绍你自己"}`)
	if res.Verdict != "ok" || res.Checks[0].Expected != "" {
		t.Fatalf("verdict=%s expected=%q", res.Verdict, res.Checks[0].Expected)
	}
}

// TestProbeReasoningEffort 传入 none 时透传到上游，并回显本次档位。
func TestProbeReasoningEffort(t *testing.T) {
	srv := fakeChat(t, func(body map[string]any) {
		if body["reasoning_effort"] != "none" {
			t.Errorf("reasoning_effort = %v，期望 none", body["reasoning_effort"])
		}
	}, chatResponse("DeepSeek-V4.1-Flash", "4", "", 1))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"text","reasoning_effort":"none"}`)
	if res.ReasoningEffort != "none" {
		t.Errorf("回复未回显 thinking 档位: %q", res.ReasoningEffort)
	}
}

// TestProbeCustomImageURL 自定义图片 URL 原样进请求，响应里回显同一个 URL。
func TestProbeCustomImageURL(t *testing.T) {
	const imgURL = "https://example.com/colors.png"
	srv := fakeChat(t, func(body map[string]any) {
		msgs, _ := body["messages"].([]any)
		msg, _ := msgs[0].(map[string]any)
		parts, _ := msg["content"].([]any)
		found := false
		for _, raw := range parts {
			p, _ := raw.(map[string]any)
			if p == nil || p["type"] != "image_url" {
				continue
			}
			u, _ := p["image_url"].(map[string]any)
			if u["url"] == imgURL {
				found = true
			}
		}
		if !found {
			t.Errorf("自定义图片 URL 未透传: %v", parts)
		}
	}, chatResponse("DeepSeek-V4.1-Flash", "橙、紫、棕", "", 1))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"vision","image_url":"`+imgURL+`"}`)
	if res.Image != imgURL {
		t.Errorf("响应图片 = %q，期望 %q", res.Image, imgURL)
	}
}

// TestProbeVisionBlindWarns 模型明确说看不到图时，属于通道限制（尤其内置 data URI），
// 判告警而不是直接判视觉失败。
func TestProbeVisionBlindWarns(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("DeepSeek-V4.1-Flash", "图片是base64文本，无法读取其中的画面内容", "", 1))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"vision"}`)
	if res.Verdict != "warn" || res.Checks[0].Level != "warn" {
		t.Fatalf("verdict=%s level=%s summary=%s", res.Verdict, res.Checks[0].Level, res.Checks[0].Summary)
	}
	if !strings.Contains(res.Checks[0].Summary, "URL") {
		t.Errorf("提示应建议改用图片 URL: %s", res.Checks[0].Summary)
	}
}

// TestProbeVisionBlindWithCustomURLFails 用户已经给了公网图片 URL 还看不到图，
// 就不是 data URI 通道限制，而是视觉真的不可用。
func TestProbeVisionBlindWithCustomURLFails(t *testing.T) {
	srv := fakeChat(t, nil, chatResponse("DeepSeek-V4.1-Flash", "无法解析所给的图像内容", "", 1))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"vision","image_url":"https://example.com/a.png"}`)
	if res.Verdict != "fail" || res.Checks[0].Level != "fail" {
		t.Fatalf("verdict=%s level=%s", res.Verdict, res.Checks[0].Level)
	}
}

// TestVerdictOfWarningOnly 只有告警、零通过时，不应该说「基本可用」。
func TestVerdictOfWarningOnly(t *testing.T) {
	v, s := verdictOf([]*probeCheck{{Level: "warn"}})
	if v != "warn" || !strings.Contains(s, "告警") {
		t.Fatalf("verdict=%s summary=%s", v, s)
	}
}

// TestProbe401MarksSessionSuspect 401 是账号级事实：测活失败要进存疑复核。
func TestProbe401MarksSessionSuspect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"session invalid"}}`))
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"DeepSeek-V4.1-Flash","mode":"both"}`)
	if res.Verdict != "fail" || len(res.Checks) != 1 {
		t.Fatalf("verdict=%s checks=%d，401 后不该再发第二个请求", res.Verdict, len(res.Checks))
	}
	if res.Checks[0].HTTPStatus != http.StatusUnauthorized {
		t.Errorf("http_status = %d", res.Checks[0].HTTPStatus)
	}
	if !p.Get("acc1").InSessionSuspect() {
		t.Error("401 后账号应进入登录态存疑")
	}
}

// TestProbe404DoesNotBlacklistModel 手工探测 404 不应把模型写进全池下架台账。
func TestProbe404DoesNotBlacklistModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"该模型暂未开放"}}`))
	}))
	defer srv.Close()

	pl, p := newTestPanelUpstream(t, srv.URL)
	_, res := probePost(t, pl, `{"model":"not-there","mode":"text"}`)
	if res.Verdict != "fail" {
		t.Fatalf("verdict = %s", res.Verdict)
	}
	if p.ModelDead("not-there") {
		t.Error("404 测活不应产生模型级下架台账")
	}
	if n := p.Get("acc1").Failures; n != 0 {
		t.Errorf("Failures = %d，404 是模型问题，不应污染账号", n)
	}
}

// TestProbeRejectsImageModel 图像生成模型不能走对话测活。
func TestProbeRejectsImageModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"Hy-Image-3.5-preview","name":"Hy image","type":"image","protocol":"openai_chat"}]}`))
			return
		}
		t.Errorf("不该打到 %s", r.URL.Path)
	}))
	defer srv.Close()

	pl, _ := newTestPanelUpstream(t, srv.URL)
	pl.models.Get(context.Background(), "sess-1")
	code, _ := probePost(t, pl, `{"model":"Hy-Image-3.5-preview","mode":"text"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", code)
	}
}

// TestProbeVisionPromptDoesNotLeakAnswer 视觉提问不能把期望颜色写进 prompt，
// 否则看不见图的模型也能照抄出期望颜色，测活就失去意义。
func TestProbeVisionPromptDoesNotLeakAnswer(t *testing.T) {
	for _, names := range probeVisionColors {
		for _, c := range names {
			if strings.Contains(strings.ToLower(probeVisionPrompt), strings.ToLower(c)) {
				t.Errorf("视觉 prompt 泄漏了期望颜色 %q：%s", c, probeVisionPrompt)
			}
		}
	}
}

// TestProbeFrontendWired 前端接线自检：行内按钮 + 弹窗元素都在。
func TestProbeFrontendWired(t *testing.T) {
	html := string(indexHTML)
	js := string(appJS)
	for _, id := range []string{"probeVeil", "probeAcc", "probeModel", "probeModelList",
		"probeMode", "probePrompt", "probeImageUrl", "probeNoThink", "probeHint", "probeResult", "btnProbeClose", "btnProbeRun"} {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s", id)
		}
	}
	if !strings.Contains(js, `data-a="probe"`) {
		t.Error("账号行缺少测活按钮")
	}
	if !strings.Contains(js, "'/probe'") && !strings.Contains(js, `'/probe'`) &&
		!strings.Contains(js, `+ '/probe'`) {
		t.Error("前端没有接测活接口")
	}
}
