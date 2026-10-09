package web

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// 面板国际化（中/英）完整性门禁。架构：中文原文即键（gettext 风格），
// i18n-en.json 是唯一字典；页面 t("中文") 与启动期 DOM 遍历（静态文本节点/
// title/placeholder）都按 trimmed 原文查键。本测试静态复刻这两条路径的取键
// 规则，漏翻译 = 提交即红，不靠人眼扫页面：
//  1. 字典可解析、值无中文（真翻译过）、无空键空值；
//  2. 静态壳（去 script/style/注释后）里每个含汉字的标签间文本与
//     title/placeholder 属性值都有键；
//  3. 脚本里每个 t("…") 实参都有键；
//  4. 脚本去注释、去 t("…") 调用后不再有裸露汉字（= 所有 UI 中文都走 t()）。
//
// 设备日志/数据不在此列（永不翻译）。
func parseI18nDict(t *testing.T) map[string]string {
	t.Helper()
	var d map[string]string
	if err := json.Unmarshal(i18nEN, &d); err != nil {
		t.Fatalf("i18n-en.json 解析失败: %v", err)
	}
	return d
}

func TestI18nDictQuality(t *testing.T) {
	d := parseI18nDict(t)
	if len(d) < 50 {
		t.Fatalf("字典只有 %d 键——不可能覆盖面板，检查生成流程", len(d))
	}
	han := regexp.MustCompile(`\p{Han}`)
	for k, v := range d {
		if strings.TrimSpace(k) == "" || !han.MatchString(k) {
			t.Errorf("键 %q 为空或不含汉字（键应为中文原文）", k)
		}
		if strings.TrimSpace(v) == "" {
			t.Errorf("键 %q 的英文翻译为空", k)
		}
		if han.MatchString(v) {
			t.Errorf("键 %q 的翻译 %q 仍含汉字——未真正翻译", k, v)
		}
	}
}

// staticShell: index.html 去掉 <script>/<style>/HTML 注释后的静态部分。
func staticShell(t *testing.T) string {
	t.Helper()
	s := string(indexHTML)
	if i := strings.Index(s, "<script>"); i >= 0 {
		if j := strings.Index(s, "</script>"); j >= 0 {
			s = s[:i] + s[j+len("</script>"):]
		}
	}
	s = regexp.MustCompile(`(?s)<style>.*?</style>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(s, "")
	return s
}

func TestI18nStaticShellCovered(t *testing.T) {
	d := parseI18nDict(t)
	miss := 0
	// 标签间文本（DOM 文本节点的静态等价物）
	for _, m := range regexp.MustCompile(`>([^<>]*\p{Han}[^<>]*)<`).FindAllStringSubmatch(staticShell(t), -1) {
		key := strings.TrimSpace(m[1])
		if _, ok := d[key]; !ok {
			t.Errorf("静态文本 %q 没有英文翻译", key)
			miss++
		}
	}
	// title/placeholder 属性（applyStaticI18n 同样翻译这两个属性）
	for _, m := range regexp.MustCompile(`(title|placeholder)="([^"]*\p{Han}[^"]*)"`).FindAllStringSubmatch(staticShell(t), -1) {
		key := strings.TrimSpace(m[2])
		if _, ok := d[key]; !ok {
			t.Errorf("静态属性 %s=%q 没有英文翻译", m[1], key)
			miss++
		}
	}
	if miss > 20 {
		t.Fatalf("静态壳缺 %d 个翻译——先补字典再谈细节", miss)
	}
}

// tokenizeJS: 面板脚本的字符串感知词法（测试专用，非通用 JS 解析器）。
// 产出 token 序列：'c' 代码（含空白）、'm' 注释、's' 字符串（内容，不含引号，
// 附带引号种类于 text 前缀 —— 这里用独立字段更直白，见 jsTok）。模板 ${} 内
// 视为代码（花括号配平）；正则字面量按代码扫描（未转义的 // /* 语法上不可能
// 出现在正则体内，不会被误判成注释——见转换器里的论证）。
type jsTok struct {
	kind byte   // 'c' 代码 | 'm' 注释 | 's' 字符串
	q    byte   // 字符串引号种类（' " `），kind=='s' 时有效
	text string // 内容：字符串为去引号原文，注释为原文，代码为原文
}

func tokenizeJS(src string) []jsTok {
	var toks []jsTok
	var code strings.Builder
	flush := func() {
		if code.Len() > 0 {
			toks = append(toks, jsTok{kind: 'c', text: code.String()})
			code.Reset()
		}
	}
	type frame struct {
		str   bool
		q     byte
		depth int // ${} 内花括号深度；str=false 时为代码态
	}
	stack := []frame{{}}
	buf := ""
	i, n := 0, len(src)
	for i < n {
		f := &stack[len(stack)-1]
		c := src[i]
		if !f.str {
			if c == '/' && i+1 < n && src[i+1] == '/' {
				j := strings.IndexByte(src[i:], '\n')
				if j < 0 {
					j = n - i
				}
				flush()
				toks = append(toks, jsTok{kind: 'm', text: src[i : i+j]})
				i += j
				continue
			}
			if c == '/' && i+1 < n && src[i+1] == '*' {
				j := strings.Index(src[i:], "*/")
				if j < 0 {
					j = n - i
				} else {
					j += 2
				}
				flush()
				toks = append(toks, jsTok{kind: 'm', text: src[i : i+j]})
				i += j
				continue
			}
			if c == '"' || c == '\'' || c == '`' {
				flush()
				stack = append(stack, frame{str: true, q: c})
				i++
				continue
			}
			if f.depth > 0 { // 模板 ${} 内：花括号配平
				if c == '{' {
					f.depth++
				} else if c == '}' && f.depth == 1 {
					code.WriteByte(c)
					stack = stack[:len(stack)-1]
					i++
					continue
				} else if c == '}' {
					f.depth--
				}
			}
			code.WriteByte(c)
			i++
			continue
		}
		// 字符串态
		if c == '\\' && i+1 < n {
			buf += src[i : i+2]
			i += 2
			continue
		}
		if f.q == '`' && c == '$' && i+1 < n && src[i+1] == '{' {
			toks = append(toks, jsTok{kind: 's', q: '`', text: buf})
			buf = ""
			code.WriteString("${")
			stack = append(stack, frame{depth: 1})
			i += 2
			continue
		}
		if c == f.q {
			toks = append(toks, jsTok{kind: 's', q: f.q, text: buf})
			buf = ""
			stack = stack[:len(stack)-1]
			i++
			continue
		}
		buf += string(c)
		i++
	}
	if buf != "" || len(stack) != 1 {
		panic("tokenizeJS: 词法未闭合（脚本损坏）")
	}
	flush()
	return toks
}

func scriptSource(t *testing.T) string {
	t.Helper()
	s := string(indexHTML)
	i := strings.Index(s, "<script>")
	j := strings.LastIndex(s, "</script>")
	if i < 0 || j < 0 {
		t.Fatal("index.html 缺 <script> 区块")
	}
	return s[i+len("<script>") : j]
}

// i18nExempt: 允许不经字典的 t() 实参——语言切换按钮显示目标语言的
// 原文名（英文模式显示"中文"），翻译它反而错误。
var i18nExempt = map[string]bool{"中文": true}

// 精确属性：代码态无汉字；注释态随便；含汉字的字符串必须是 t("…") 的实参
// （前一代码 token 以 `t(` 结尾），且键在字典里。t( 与引号之间不允许空白。
func TestI18nScriptStringsCovered(t *testing.T) {
	d := parseI18nDict(t)
	toks := tokenizeJS(scriptSource(t))
	for idx, tok := range toks {
		if tok.kind != 's' || !hanRe.MatchString(tok.text) {
			continue
		}
		inT := false
		for k := idx - 1; k >= 0 && toks[k].kind != 'm'; k-- {
			if toks[k].kind == 'c' {
				inT = strings.HasSuffix(strings.TrimRight(toks[k].text, " \t\n"), "t(")
				break
			}
		}
		if !inT {
			t.Errorf("字符串 %q 含汉字但不是 t() 实参——裸 UI 文案", tok.text)
			continue
		}
		if tok.q != '"' {
			t.Errorf("t() 实参 %q 引号种类 %q（应为双引号）", tok.text, string(tok.q))
		}
		if i18nExempt[tok.text] {
			continue
		}
		if _, ok := d[tok.text]; !ok {
			t.Errorf("t(%q) 的实参没有英文翻译", tok.text)
		}
	}
}

func TestI18nNoBareHanInScript(t *testing.T) {
	for _, tok := range tokenizeJS(scriptSource(t)) {
		if tok.kind == 'c' && hanRe.MatchString(tok.text) {
			// 代码态允许的唯一汉字来源：都不允许（中文只该出现在字符串/注释里）
			t.Errorf("代码段 %q 混入汉字", firstHanLine(tok.text))
		}
	}
}

func firstHanLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if hanRe.MatchString(l) {
			return strings.TrimSpace(l)
		}
	}
	return strings.TrimSpace(s)
}

var hanRe = regexp.MustCompile(`\p{Han}`)

func TestI18nRuntimeWired(t *testing.T) {
	html := string(indexHTML)
	for _, need := range []string{
		`id="btnLang"`,                    // 头部语言切换按钮
		`localStorage.getItem("st_lang")`, // 语言持久化
		`navigator.language`,              // 首访自动检测
		`function applyStaticI18n(`,       // 静态壳翻译入口
	} {
		if !strings.Contains(html, need) {
			t.Errorf("index.html 缺 i18n 运行时要素: %s", need)
		}
	}
	// 服务端注入：字典与指纹都进 <head>，指纹覆盖字典（改字典也触发面板自愈）
	served := injectPanel(indexHTML, i18nEN, "deadbeef")
	if !strings.Contains(string(served), `window.I18N_EN=`) {
		t.Error("injectPanel 未注入 I18N_EN 字典")
	}
	if !strings.Contains(string(served), `name="panel-rev" content="deadbeef"`) {
		t.Error("injectPanel 未注入 panel-rev 指纹")
	}
	rev1 := panelRevOf(indexHTML, []byte(`{}`))
	rev2 := panelRevOf(indexHTML, []byte(`{"x":"y"}`))
	if rev1 == rev2 {
		t.Error("panelRev 未覆盖字典字节——改字典不会触发旧页面自愈刷新")
	}
}
