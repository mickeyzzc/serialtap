package web

// 面板 XSS 静态门禁（#66 回禁）。设备侧可控事实（设备名/状态/tty/身份摘要/
// 日志行/mesh 字段）最终都汇进赋给 innerHTML 的模板字符串字面量。本文件
// 对面板脚本源码做字符串感知扫描，把每个 HTML 模板里的 `${…}` 插值拎出来，
// 要求其表达式"构造上安全"：
//   - 整体被安全 helper 包裹（esc/cfgEsc/cfgRow/t/maskVal/fmtKB/fmtAgo/
//     stDotColor/devColor/CSS.escape/encodeURIComponent/hitLine——esc 已含
//     引号转义，属性值同样安全）；
//   - 或是字符串/数字字面量、数值成员（*.length 等）与白名单变量的
//     +/||/&&/三元组合（白名单 = 返回预转义 HTML 或纯数字的局部变量）。
// 新写插值若不满足，提交即红——按报错包 esc() 或扩白名单；扩白名单前先
// 想清楚该变量是否真的不含设备可控文本。纯字符串拼接（非模板插值）不在
// 此门禁覆盖内，已知 sink（facts 表格、时间线刻度）由 TestKnownXSSTargets
// 逐点钉死。
//
// Go 测试无 DOM 执行环境，"facts 含 <img onerror=x> 渲染后仍转义"在浏览器
// 侧成立的前提 = 渲染点全部 esc() 包裹 + esc() 转义引号，两者都在下面静态
// 断言；模板插值的面上收口则由 TestTemplateInterpolationGate 兜住。
//
// 为什么不复用 i18n 测试的 tokenizeJS 提取插值：它把插值里的字符串拆成独立
// token，且 code 不在插值边界 flush——`)}${` 会与后续字面部分缝合进同一
// token，token 流顺序与源码顺序不一致，插值表达式无法可靠重建；这里对源码
// 单遍扫描。正则字面量按"前一有效字符"启发式识别（除号前必是标识符/字面
// 量/右括号，正则前必是运算符/开括号——面板脚本全部命中；esc() 的引号转义
// 因此用 split/join 而非含引号的正则，两处约定互相咬合）。

import (
	"regexp"
	"strings"
	"testing"
)

// —— 源码扫描器：字符串/模板/注释/正则感知 ——

// scanStr: s[i] 为开引号（' " `），返回闭引号后下标；处理 \ 转义；反引号内
// `${…}` 为代码段，递归 scanBrace 跳过（嵌套模板因此被整体跨过）。
func scanStr(s string, i int) int {
	q := s[i]
	i++
	for i < len(s) {
		if s[i] == '\\' {
			i += 2
			continue
		}
		if q == '`' && s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			i = scanBrace(s, i+2)
			continue
		}
		if s[i] == q {
			return i + 1
		}
		i++
	}
	return len(s)
}

// scanBrace: s[i] 位于 `${` 的 '{' 之后，返回配平 '}' 之后下标；途中跳过
// 字符串与（启发式识别的）正则字面量。
func scanBrace(s string, i int) int {
	depth, prev := 1, byte('{')
	for i < len(s) && depth > 0 {
		c := s[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			i = scanStr(s, i)
			prev = '"'
			continue
		case c == '/' && regexAllowedAfter(prev) && i+1 < len(s):
			i = scanRegex(s, i)
			prev = '/'
			continue
		case c == '{':
			depth++
		case c == '}':
			depth--
		}
		if c > ' ' {
			prev = c
		}
		i++
	}
	return i
}

// regexAllowedAfter: 除号/正则消歧——前一有效字符为运算符、开括号、分隔符
// （或行首）时 '/' 开正则；为标识符/字面量/右括号时是除法。
func regexAllowedAfter(prev byte) bool {
	switch prev {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '+', '-', '*', '%',
		'^', '~', '<', '>', '{', '}', ';', '/', '"', '\'':
		return true
	}
	return false
}

// scanRegex: s[i]=='/'，跳过正则字面量（字符类内 '/' 不闭合；遇换行按
// "不是正则"安全兜底返回）。
func scanRegex(s string, i int) int {
	i++
	inClass := false
	for i < len(s) {
		c := s[i]
		if c == '\\' {
			i += 2
			continue
		}
		if inClass {
			if c == ']' {
				inClass = false
			}
		} else if c == '[' {
			inClass = true
		} else if c == '/' {
			i++
			for i < len(s) && s[i] >= 'a' && s[i] <= 'z' { // flags（gi 等）
				i++
			}
			return i
		} else if c == '\n' {
			return i
		}
		i++
	}
	return len(s)
}

// tmplInterp: 一个 `${…}` 插值（表达式 + 前邻字面部分，报错定位用）。
type tmplInterp struct {
	expr   string
	before string
}

// tmplGroup: 一个模板字面量（全部字面部分 + 逐段插值）。
type tmplGroup struct {
	parts  []string
	inters []tmplInterp
}

// scanTemplate: i 位于开反引号之后；返回模板组与闭反引号后下标。插值内嵌
// 套模板由 scanBrace→scanStr 整体跨过（其插值在叶子检查时经表达式源码递归）。
func scanTemplate(src string, i int) (tmplGroup, int) {
	g := tmplGroup{}
	var buf strings.Builder
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src):
			buf.WriteString(src[i : i+2])
			i += 2
		case c == '`':
			g.parts = append(g.parts, buf.String())
			return g, i + 1
		case c == '$' && i+1 < len(src) && src[i+1] == '{':
			part := buf.String()
			g.parts = append(g.parts, part)
			j := scanBrace(src, i+2)
			g.inters = append(g.inters, tmplInterp{src[i+2 : j-1], part})
			buf.Reset()
			i = j
		default:
			buf.WriteByte(c)
			i++
		}
	}
	g.parts = append(g.parts, buf.String())
	return g, len(src)
}

// scanTemplates: 扫描脚本源码，产出全部模板字面量（顶层）。
func scanTemplates(src string) []tmplGroup {
	var groups []tmplGroup
	i, n := 0, len(src)
	prev := byte(0)
	for i < n {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = n - i
			}
			i += j
			continue
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := strings.Index(src[i:], "*/")
			if j < 0 {
				j = n - i
			} else {
				j += 2
			}
			i += j
			continue
		case c == '\'' || c == '"':
			i = scanStr(src, i)
			prev = '"'
			continue
		case c == '`':
			g, next := scanTemplate(src, i+1)
			groups = append(groups, g)
			i, prev = next, '`'
			continue
		case c == '/' && regexAllowedAfter(prev) && i+1 < n:
			i = scanRegex(src, i)
			prev = '/'
			continue
		}
		if c > ' ' {
			prev = c
		}
		i++
	}
	return groups
}

// —— 插值表达式安全性判定 ——

// interpSafeHelper: 整体包裹即安全的 helper 前缀（含开括号，防 test( 误配 t(；
// ticks.join 只拼已转义的刻度 div 数组）。
var interpSafeHelper = []string{
	"esc(", "t(", "cfgEsc(", "cfgRow(", "maskVal(", "fmtKB(", "fmtAgo(",
	"stDotColor(", "devColor(", "CSS.escape(", "encodeURIComponent(", "hitLine(",
	"ticks.join(",
}

// interpSafeVar: 白名单变量。tag/html = esc 组合的预转义 HTML；label/val =
// cfgRow 的契约参数（val 允许故意含内联标记，由调用侧 cfgEsc/字面量构成）；
// k2 = 配置速览行标签（t()/字面量构成）；cls/x/i = 纯数字或固定字面量；
// accessTxt = t() 三元派生。
var interpSafeVar = map[string]bool{
	"accessTxt": true, "label": true, "val": true, "tag": true, "html": true,
	"k2": true, "cls": true, "i": true, "x": true,
}

// interpNumericMember: 点分标识符链（如 mesh.peers.length）。
var interpNumericMember = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*(\.[A-Za-z_$][A-Za-z0-9_$]*)*$`)

var interpNumericLiteral = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// htmlTagRe: 模板字面部分含 HTML 标签 → 视为 HTML 模板（插值进 innerHTML）。
var htmlTagRe = regexp.MustCompile(`<[/!]?[A-Za-z]`)

// splitTernary: 顶层 `cond ? A : B` → 只渲染 A/B，cond 不检查。
func splitTernary(expr string) (then, els string, ok bool) {
	depth, qpos := 0, -1
	i, n := 0, len(expr)
	for i < n {
		c := expr[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			i = scanStr(expr, i) // 闭引号后一字符（无后置 i++，不会跳字）
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == '?' && depth == 0 && qpos < 0:
			if i+1 < n && (expr[i+1] == '.' || expr[i+1] == '?') { // ?. ?? 不切
				i += 2
				continue
			}
			qpos = i
		case c == ':' && depth == 0 && qpos >= 0:
			return expr[qpos+1 : i], expr[i+1:], true
		}
		i++
	}
	return "", "", false
}

// splitTopOps: 顶层（不在字符串/括号内）按 + / || / && 切分（三种运算的
// 结果都会被渲染）。
func splitTopOps(expr string) []string {
	var segs []string
	last := 0
	depth := 0
	i, n := 0, len(expr)
	for i < n {
		c := expr[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			i = scanStr(expr, i) // 无后置 i++，不会跳过字符串后的运算符
			continue
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == '+' && depth == 0:
			if i+1 < n && (expr[i+1] == '+' || expr[i+1] == '=') { // ++ += 不切
				i += 2
				continue
			}
			segs = append(segs, expr[last:i])
			i++
			for i < n && expr[i] == ' ' {
				i++
			}
			last = i
			continue
		case (c == '|' || c == '&') && depth == 0 && i+1 < n && expr[i+1] == c:
			segs = append(segs, expr[last:i])
			i += 2
			for i < n && expr[i] == ' ' {
				i++
			}
			last = i
			continue
		}
		i++
	}
	return append(segs, expr[last:])
}

// closesAtEnd: seg[open] 为 '('，验证与其配平的 ')' 恰为末字符（helper 整包）。
func closesAtEnd(seg string, open int) bool {
	depth := 0
	for i := open; i < len(seg); {
		switch {
		case seg[i] == '\'' || seg[i] == '"' || seg[i] == '`':
			i = scanStr(seg, i) // 无后置 i++，闭引号后的 ')' 不会被跳过
			continue
		case seg[i] == '(' || seg[i] == '[' || seg[i] == '{':
			depth++
		case seg[i] == ')' || seg[i] == ']' || seg[i] == '}':
			depth--
			if depth == 0 {
				return i == len(seg)-1
			}
		}
		i++
	}
	return false
}

// checkInterpExpr: 插值表达式是否构造上安全（三元只查分支；+/||/&& 链逐段
// 查叶子；叶子 = 字面量 / 安全 helper / 白名单）。
func checkInterpExpr(expr string, depth int, report func(string)) bool {
	if depth > 5 {
		report("表达式嵌套过深: ${" + expr + "}")
		return false
	}
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true
	}
	if then, els, ok := splitTernary(expr); ok {
		return checkInterpExpr(then, depth+1, report) && checkInterpExpr(els, depth+1, report)
	}
	ok := true
	for _, seg := range splitTopOps(expr) {
		if !checkInterpLeaf(strings.TrimSpace(seg), depth, report) {
			ok = false
		}
	}
	return ok
}

func checkInterpLeaf(seg string, depth int, report func(string)) bool {
	if seg == "" {
		return true
	}
	switch seg[0] {
	case '"', '\'':
		return scanStr(seg, 0) == len(seg) // 整段一个字符串字面量
	case '`':
		if strings.HasSuffix(seg, "`") && len(seg) >= 2 {
			return checkInterpTemplate(seg[1:len(seg)-1], depth+1, report)
		}
	}
	if interpNumericLiteral.MatchString(seg) {
		return true
	}
	for _, h := range interpSafeHelper {
		if strings.HasPrefix(seg, h) && closesAtEnd(seg, len(h)-1) {
			return true
		}
	}
	if interpSafeVar[seg] {
		return true
	}
	if interpNumericMember.MatchString(seg) {
		last := seg[strings.LastIndex(seg, ".")+1:]
		if last == "length" || seg == "p.latency_ms" { // 已知数值成员
			return true
		}
	}
	report("插值表达式未包安全 helper 且不在白名单: ${" + seg + "}")
	return false
}

// checkInterpTemplate: 嵌套模板字面量——其内部插值同样逐个检查。
func checkInterpTemplate(body string, depth int, report func(string)) bool {
	ok := true
	i, n := 0, len(body)
	for i < n {
		if body[i] == '\\' {
			i += 2
			continue
		}
		if body[i] == '$' && i+1 < n && body[i+1] == '{' {
			j := scanBrace(body, i+2)
			if !checkInterpExpr(body[i+2:j-1], depth, report) {
				ok = false
			}
			i = j
			continue
		}
		i++
	}
	return ok
}

// TestTemplateInterpolationGate: HTML 模板（字面部分含标签、即会进 innerHTML
// 的模板）里的每个 `${…}` 插值必须构造上安全。
func TestTemplateInterpolationGate(t *testing.T) {
	for _, g := range scanTemplates(scriptSource(t)) {
		html := false
		for _, p := range g.parts {
			if htmlTagRe.MatchString(p) {
				html = true
				break
			}
		}
		if !html {
			continue // URL/选择器/纯文本模板（textContent 等），不在 innerHTML 管线
		}
		for _, it := range g.inters {
			tail := it.before
			if len(tail) > 40 {
				tail = "…" + tail[len(tail)-40:]
			}
			checkInterpExpr(it.expr, 0, func(msg string) {
				t.Errorf("%s\n  上下文（插值前的字面部分末尾）: %q", msg, tail)
			})
		}
	}
}

// TestKnownXSSTargets: 已知 sink 逐点钉死（模板插值之外的字符串拼接点 +
// esc() 本体的引号转义——属性插值的安全前提）。
func TestKnownXSSTargets(t *testing.T) {
	js := scriptSource(t)

	// 设备卡身份摘要行（#66 主 sink）：identLine 输出 = facts 拼串，必须 esc
	if strings.Contains(js, "${identLine(") {
		t.Error("存在 ${identLine(…} 未包 esc()——身份摘要直插 innerHTML")
	}
	if !strings.Contains(js, "${esc(identLine(d.info))}") {
		t.Error("设备卡身份行应为 ${esc(identLine(d.info))}")
	}

	// 事件时间线刻度 title 属性（双引号属性 + 设备日志行内容）
	if !regexp.MustCompile(`title="\$\{esc\(l\.slice\(1,\s*160\)\)\}"`).MatchString(js) {
		t.Error("时间线刻度 title 属性必须为 esc(l.slice(1,160)) 包裹")
	}

	// facts 证据/来源列：单引号属性 + 原始日志行——esc 必须已转义引号才不破出
	for _, need := range []string{
		`title='"+esc(f.source||"")+"'`,
		`'<td class="fv">'+esc(f.value)+'</td>'`,
	} {
		if !strings.Contains(js, need) {
			t.Errorf("facts 表格缺转义写法: %s", need)
		}
	}

	// esc() 本体：textContent→innerHTML 之后必须补引号转义
	idx := strings.Index(js, "const esc ")
	if idx < 0 {
		t.Fatal("找不到 esc 定义")
	}
	def := js[idx:]
	if len(def) > 400 {
		def = def[:400]
	}
	for _, need := range []string{`.split('"').join("&quot;")`, `.split("'").join("&#39;")`} {
		if !strings.Contains(def, need) {
			t.Errorf("esc() 缺引号转义 %s——属性插值可被引号逃逸；定义: %s", need, def)
		}
	}
}

// TestI18nValuesHTMLSafe: t() 输出不经转义进 innerHTML/属性，字典英文值不得
// 含 <（元素内容注入）或 "（双引号属性破出）。撇号 ' 只用于文本节点/元素
// 内容，当前无单引号属性插值点，故不禁。
func TestI18nValuesHTMLSafe(t *testing.T) {
	for k, v := range parseI18nDict(t) {
		if strings.ContainsAny(v, "<\"") {
			t.Errorf("键 %q 的英文值含 < 或 \"（t() 输出不转义直插 HTML）: %q", k, v)
		}
	}
}
