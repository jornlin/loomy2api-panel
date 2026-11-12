// frontend_test.go 前端接线自检：页面元素与 app.js 的选择器必须对得上。
//
// 这类漂移不会让 Go 编译失败，也不会让任何后端测试变红——只会在浏览器里变成
// 「点按钮没反应」或「某块永远是空的」。所以在这里用最小代价静态对账一次。
package panel

import (
	"regexp"
	"strings"
	"testing"
)

var (
	reIDRef   = regexp.MustCompile(`\$\('#([A-Za-z0-9_-]+)'`)
	reIDDef   = regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	reNavView = regexp.MustCompile(`data-view="([a-z]+)"`)
	reSection = regexp.MustCompile(`id="view-([a-z]+)"`)
	reInline  = regexp.MustCompile(`\son(click|submit|change|input)=`)
)

// TestSelectorsResolve app.js 里每个 $('#id') 都能在 index.html 里找到。
func TestSelectorsResolve(t *testing.T) {
	html := string(indexHTML)
	defined := map[string]bool{}
	for _, m := range reIDDef.FindAllStringSubmatch(html, -1) {
		defined[m[1]] = true
	}
	for _, m := range reIDRef.FindAllStringSubmatch(string(appJS), -1) {
		if !defined[m[1]] {
			t.Errorf("app.js 引用了 #%s，但 index.html 里没有这个 id", m[1])
		}
	}
}

// TestNavMatchesViews 每个导航项都要有对应的视图区块，反之亦然。
func TestNavMatchesViews(t *testing.T) {
	html := string(indexHTML)
	nav := map[string]bool{}
	for _, m := range reNavView.FindAllStringSubmatch(html, -1) {
		nav[m[1]] = true
	}
	sec := map[string]bool{}
	for _, m := range reSection.FindAllStringSubmatch(html, -1) {
		sec[m[1]] = true
	}
	for v := range nav {
		if !sec[v] {
			t.Errorf("导航 %q 没有 #view-%s 区块", v, v)
		}
	}
	for v := range sec {
		if !nav[v] {
			t.Errorf("#view-%s 没有对应导航项", v)
		}
	}
}

// TestThemePreloadBeforeStyle 主题预热脚本必须在样式表之前，否则刷新会先闪深色。
func TestThemePreloadBeforeStyle(t *testing.T) {
	html := string(indexHTML)
	i := strings.Index(html, `src="theme.js?`) // 运行时带内容指纹，源码里是无查询串形态
	j := strings.Index(html, "<style>")
	if i < 0 || j < 0 {
		t.Fatal("缺少 theme.js 或内联样式块")
	}
	if i > j {
		t.Error("theme.js 必须在 <style> 之前同步加载")
	}
	if !strings.Contains(string(themeJS), "loomy2api.theme") {
		t.Error("theme.js 应读 localStorage['loomy2api.theme']（与 app.js 一致）")
	}
}

// TestNoInlineHandlers CSP 是 script-src 'self'，内联事件处理器会被直接拦掉。
func TestNoInlineHandlers(t *testing.T) {
	if m := reInline.FindString(string(indexHTML)); m != "" {
		t.Errorf("存在内联事件处理器 %q，CSP 会拦", strings.TrimSpace(m))
	}
	if strings.Contains(string(indexHTML), "<script>") {
		t.Error("不应有内联脚本")
	}
}

// TestPagerNeverTrapsSizeChoice 分页条不能既是「唯一能改每页条数的地方」、又在
// 只有一页时整条消失：选了 200 条/页 + 166 个账号 = 单页，分页条没了就再也切不
// 回来（实测踩过这个死局）。所以：只有列表为空才移除分页条；单页时可以省掉页码
// 按钮，但「每页条数」选择器必须留着。
func TestPagerNeverTrapsSizeChoice(t *testing.T) {
	src := string(appJS)
	if strings.Contains(src, "total <= st.size") {
		t.Error("分页条不该因为「一页装得下」就移除 —— 每页条数的选择器会长在上面，一起消失就没法切回")
	}
	if !strings.Contains(src, "total === 0") {
		t.Error("分页条只应在列表为空时移除")
	}
	i := strings.Index(src, "function pgHtml(")
	if i < 0 {
		t.Fatal("app.js 里找不到 pgHtml")
	}
	body := src[i:]
	if j := strings.Index(body, "\n}"); j > 0 {
		body = body[:j]
	}
	if !strings.Contains(body, "data-pgsize") {
		t.Error("pgHtml 必须无条件渲染每页条数选择器")
	}
	if strings.Contains(body, "if (pages <= 1) return") {
		t.Error("pgHtml 不该在单页时整体返回，否则选择器跟着消失")
	}
}

// TestStatsStripSingleRow：KPI 条靠 auto-fit 排成一行，写死列数会让多出来的格
// 单独掉到第二行，破坏与基准一致的「单行等分 + 细分割线」。
func TestStatsStripSingleRow(t *testing.T) {
	html := string(indexHTMLRaw)
	i := strings.Index(html, ".stats {")
	if i < 0 {
		t.Fatal("index.html 里找不到 .stats 规则")
	}
	rule := html[i : i+strings.Index(html[i:], "}")]
	if !strings.Contains(rule, "auto-fit") {
		t.Errorf(".stats 应使用 auto-fit 自适应列数，实际规则：%s", rule)
	}
}

// TestEveryPagerHasRenderer 每个用了 pgApply 的列表都必须注册 PG_RENDER.<key>。
//
// 这条漂移的后果很具体：点「下一页」/点表头/改每页条数时，事件处理器只更新分页
// 状态再调 PG_RENDER[key]() —— 没注册就等于什么都没发生，表现是「分页不管用、
// 排序没反应」（邀请码页首版就漏了这一行）。
func TestEveryPagerHasRenderer(t *testing.T) {
	src := string(appJS)
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`pgApply\('[A-Za-z0-9]+',\s*'([A-Za-z0-9]+)'`).FindAllStringSubmatch(src, -1) {
		used[m[1]] = true
	}
	registered := map[string]bool{}
	for _, m := range regexp.MustCompile(`PG_RENDER\.([A-Za-z0-9]+)\s*=`).FindAllStringSubmatch(src, -1) {
		registered[m[1]] = true
	}
	if len(used) < 5 {
		t.Fatalf("没解析到几个分页表（正则失效？）: %v", used)
	}
	for k := range used {
		if !registered[k] {
			t.Errorf("分页 key %q 没有 PG_RENDER.%s —— 分页/排序点了不会有反应", k, k)
		}
	}
}

// TestInviteSortKeysResolve 邀请表的每个可排序列都要有对应的取键函数。
//
// sortApply(key, rows, getters, ...) 用 getters[st.field] 取值，取不到就退回默认列 ——
// 表头点了没反应、或者点了排的是别的列，都从这里来。
func TestInviteSortKeysResolve(t *testing.T) {
	html := string(indexHTML)
	src := string(appJS)
	tbl := regexp.MustCompile(`(?s)data-sortkey="inv".*?</table>`).FindString(html)
	if tbl == "" {
		t.Fatal("找不到邀请表（data-sortkey=\"inv\"）")
	}
	fields := []string{}
	for _, m := range regexp.MustCompile(`data-sort="([A-Za-z0-9]+)"`).FindAllStringSubmatch(tbl, -1) {
		fields = append(fields, m[1])
	}
	if len(fields) == 0 {
		t.Fatal("邀请表没有任何可排序列")
	}
	block := regexp.MustCompile(`(?s)const INV_GET = \{(.*?)\n\};`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("app.js 里找不到 INV_GET")
	}
	has := map[string]bool{}
	for _, m := range regexp.MustCompile(`([A-Za-z0-9]+):\s*a =>`).FindAllStringSubmatch(block[1], -1) {
		has[m[1]] = true
	}
	for _, f := range fields {
		if !has[f] {
			t.Errorf("表头可排序列 %q 在 INV_GET 里没有取键函数", f)
		}
	}
}
