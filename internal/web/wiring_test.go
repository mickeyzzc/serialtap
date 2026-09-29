package web

import (
	"regexp"
	"strings"
	"testing"
)

// 面板接线一致性：JS 里引用的每个 [data-a=…] 选择器与 $("id") 元素都必须
// 在 index.html 里真实存在。背景（2026-09-26 真机故障）：rebase 时卡片模板
// 换掉了 data-a="view" 按钮、JS 绑定行残留 → querySelector 返回 null →
// onclick 赋值抛 TypeError → renderCards 中途夭折，卡片区与设备下拉全空
// （异常藏在 fetch 的 unhandled rejection 里，页面无任何报错提示）。
func TestIndexHTMLWiring(t *testing.T) {
	html := string(indexHTML)

	// [data-a="X"]：每次 querySelector 引用之外，模板里必须还有一处真实标记
	// （引用本身也包含该子串，所以要求出现次数 > 引用次数）
	selRef := regexp.MustCompile(`querySelector\('\[data-a="([^"]+)"\]'\)`)
	for _, m := range selRef.FindAllStringSubmatch(html, -1) {
		name := m[1]
		refs := strings.Count(html, `querySelector('[data-a="`+name+`"]')`)
		total := strings.Count(html, `data-a="`+name+`"`)
		if total <= refs {
			t.Errorf("JS 绑定了 [data-a=%q]（%d 处引用），但模板里没有对应按钮/元素——querySelector 将返回 null，renderCards 会在赋值 onclick 时抛错", name, refs)
		}
	}

	// $("id")：面板脚本查的元素 id 必须存在
	idRef := regexp.MustCompile(`\$\("([A-Za-z][A-Za-z0-9_-]*)"\)`)
	for _, m := range idRef.FindAllStringSubmatch(html, -1) {
		id := m[1]
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("JS 查找 $(\"%s\")，但页面里没有 id=\"%s\" 的元素", id, id)
		}
	}
}
