package webtools

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Document is an HTML page converted to Markdown.
type Document struct {
	Title    string
	Markdown string
}

// HTMLToMarkdown converts an HTML document to Markdown for a model to read.
// Scripts, styles, navigation, forms and hidden elements are dropped; when
// the page marks its main content (main, role=main or a single article),
// only that part is kept. Links and images are resolved against base (or
// the page's own <base href>).
func HTMLToMarkdown(src string, base *url.URL) (Document, error) {
	root, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return Document{}, err
	}
	c := &converter{base: base}
	if b := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Base && attr(n, "href") != "" }); b != nil {
		if u, err := c.resolve(attr(b, "href")); err == nil {
			c.base = u
		}
	}
	doc := Document{}
	if t := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Title }); t != nil {
		doc.Title = collapseSpace(textContent(t))
	}
	content := mainContent(root)
	md := strings.Join(c.blocks(content), "\n\n")
	doc.Markdown = tidy(md)
	return doc, nil
}

// mainContent picks the part of the page that holds its content.
func mainContent(root *html.Node) *html.Node {
	if m := findFirst(root, func(n *html.Node) bool {
		return n.Type == html.ElementNode && (n.DataAtom == atom.Main || attr(n, "role") == "main") && !dropped(n)
	}); m != nil {
		return m
	}
	var articles []*html.Node
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.DataAtom == atom.Article && !dropped(n) {
			articles = append(articles, n)
			return false
		}
		return true
	})
	if len(articles) == 1 {
		return articles[0]
	}
	if b := findFirst(root, func(n *html.Node) bool { return n.DataAtom == atom.Body }); b != nil {
		return b
	}
	return root
}

type converter struct {
	base *url.URL
}

// dropped reports elements whose content is never shown.
func dropped(n *html.Node) bool {
	if n.Type == html.CommentNode || n.Type == html.DoctypeNode {
		return true
	}
	if n.Type != html.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Svg, atom.Math, atom.Canvas,
		atom.Nav, atom.Footer, atom.Aside, atom.Form, atom.Button, atom.Input, atom.Select, atom.Textarea,
		atom.Option, atom.Label, atom.Iframe, atom.Object, atom.Embed, atom.Head, atom.Link, atom.Meta,
		atom.Audio, atom.Video, atom.Map, atom.Dialog:
		return true
	}
	if n.Data == "search" { // <search> is not in the atom table of every x/net release
		return true
	}
	if hasAttr(n, "hidden") || strings.EqualFold(attr(n, "aria-hidden"), "true") {
		return true
	}
	switch strings.ToLower(attr(n, "role")) {
	case "navigation", "banner", "contentinfo", "search", "complementary", "dialog", "menu", "menubar":
		return true
	}
	style := strings.ToLower(strings.ReplaceAll(attr(n, "style"), " ", ""))
	return strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden")
}

// isBlock reports elements rendered as their own Markdown block.
func isBlock(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.Address, atom.Article, atom.Blockquote, atom.Body, atom.Center, atom.Dd, atom.Details, atom.Div,
		atom.Dl, atom.Dt, atom.Fieldset, atom.Figcaption, atom.Figure, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5,
		atom.H6, atom.Header, atom.Hgroup, atom.Hr, atom.Html, atom.Li, atom.Main, atom.Ol, atom.P, atom.Pre,
		atom.Section, atom.Summary, atom.Table, atom.Tbody, atom.Td, atom.Tfoot, atom.Th, atom.Thead, atom.Tr,
		atom.Ul, atom.Caption, atom.Legend:
		return true
	}
	return false
}

// blocks renders the children of n as Markdown blocks. Runs of inline
// content between block elements become paragraphs.
func (c *converter) blocks(n *html.Node) []string {
	var out []string
	var inl strings.Builder
	flush := func() {
		if t := cleanInline(inl.String()); t != "" {
			out = append(out, t)
		}
		inl.Reset()
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if dropped(ch) {
			continue
		}
		if isBlock(ch) {
			flush()
			out = append(out, c.block(ch)...)
			continue
		}
		inl.WriteString(c.inline(ch))
	}
	flush()
	return out
}

// block renders one block element.
func (c *converter) block(n *html.Node) []string {
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		t := strings.ReplaceAll(cleanInline(c.inlineChildren(n)), "\n", " ")
		if t == "" {
			return nil
		}
		return []string{strings.Repeat("#", level) + " " + t}
	case atom.Hr:
		return []string{"---"}
	case atom.Pre:
		return []string{c.codeBlock(n)}
	case atom.Ul, atom.Ol:
		if l := c.list(n); l != "" {
			return []string{l}
		}
		return nil
	case atom.Blockquote:
		inner := strings.Join(c.blocks(n), "\n\n")
		if strings.TrimSpace(inner) == "" {
			return nil
		}
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			if l == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + l
			}
		}
		return []string{strings.Join(lines, "\n")}
	case atom.Table:
		if t := c.table(n); t != "" {
			return []string{t}
		}
		return nil
	case atom.Dl:
		return c.definitionList(n)
	case atom.Dt, atom.Summary, atom.Caption, atom.Legend:
		if t := cleanInline(c.inlineChildren(n)); t != "" {
			return []string{"**" + t + "**"}
		}
		return nil
	case atom.Figcaption:
		if t := cleanInline(c.inlineChildren(n)); t != "" {
			return []string{"*" + t + "*"}
		}
		return nil
	}
	return c.blocks(n)
}

// inlineChildren renders the children of n as inline Markdown; block
// children are flattened onto their own lines.
func (c *converter) inlineChildren(n *html.Node) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if dropped(ch) {
			continue
		}
		if isBlock(ch) {
			b.WriteString("\n" + strings.Join(c.block(ch), "\n") + "\n")
			continue
		}
		b.WriteString(c.inline(ch))
	}
	return b.String()
}

var spaceRun = regexp.MustCompile(`[ \t\r\n\f\v\x{00a0}]+`)

// inline renders a non-block node.
func (c *converter) inline(n *html.Node) string {
	switch n.Type {
	case html.TextNode:
		return spaceRun.ReplaceAllString(n.Data, " ")
	case html.ElementNode:
	default:
		return ""
	}
	if dropped(n) {
		return ""
	}
	if isBlock(n) {
		return "\n" + strings.Join(c.block(n), "\n") + "\n"
	}
	switch n.DataAtom {
	case atom.Br:
		return "\n"
	case atom.A:
		raw := c.inlineChildren(n)
		lead, trail := edgeSpace(raw)
		text := strings.TrimSpace(strings.ReplaceAll(cleanInline(raw), "\n", " "))
		href := strings.TrimSpace(attr(n, "href"))
		if href == "" || strings.HasPrefix(href, "#") {
			return text
		}
		u, err := c.resolve(href)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto") {
			return text
		}
		if text == "" {
			if hasImage(n) {
				return ""
			}
			text = u.String()
		}
		return lead + "[" + escapeBrackets(text) + "](" + linkURL(u) + ")" + trail
	case atom.Img:
		alt := collapseSpace(attr(n, "alt"))
		if alt == "" {
			return ""
		}
		src := strings.TrimSpace(attr(n, "src"))
		u, err := c.resolve(src)
		if src == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return alt
		}
		return "![" + escapeBrackets(alt) + "](" + linkURL(u) + ")"
	case atom.Code, atom.Kbd, atom.Samp, atom.Tt:
		t := spaceRun.ReplaceAllString(textContent(n), " ")
		if strings.TrimSpace(t) == "" {
			return t
		}
		return inlineCode(t)
	case atom.Strong, atom.B:
		return wrapInline(c.inlineChildren(n), "**")
	case atom.Em, atom.I, atom.Cite, atom.Var, atom.Dfn:
		return wrapInline(c.inlineChildren(n), "*")
	case atom.Del, atom.S, atom.Strike:
		return wrapInline(c.inlineChildren(n), "~~")
	case atom.Sup:
		return "^" + c.inlineChildren(n)
	case atom.Q:
		return "\"" + c.inlineChildren(n) + "\""
	}
	return c.inlineChildren(n)
}

func hasImage(n *html.Node) bool {
	return findFirst(n, func(x *html.Node) bool { return x.DataAtom == atom.Img }) != nil
}

// wrapInline puts emphasis markers around text, keeping surrounding spaces
// outside the markers so the Markdown stays valid.
func wrapInline(t, mark string) string {
	core := strings.TrimSpace(t)
	if core == "" {
		return t
	}
	lead, trail := edgeSpace(t)
	return lead + mark + strings.ReplaceAll(cleanInline(core), "\n", " ") + mark + trail
}

// edgeSpace returns a single space for each side of t that starts or ends
// with white space, so markers can sit inside it.
func edgeSpace(t string) (string, string) {
	lead, trail := "", ""
	if strings.TrimLeft(t, " \n") != t {
		lead = " "
	}
	if strings.TrimRight(t, " \n") != t {
		trail = " "
	}
	return lead, trail
}

func inlineCode(t string) string {
	fence := "`"
	for strings.Contains(t, fence) {
		fence += "`"
	}
	if strings.HasPrefix(t, "`") || strings.HasSuffix(t, "`") {
		return fence + " " + t + " " + fence
	}
	return fence + t + fence
}

// codeBlock renders <pre> verbatim in a fence, with the language taken from
// a language-* or lang-* class on the pre or its code child.
func (c *converter) codeBlock(n *html.Node) string {
	var b strings.Builder
	preText(n, &b)
	code := strings.Trim(b.String(), "\n")
	lang := codeLanguage(n)
	if lang == "" {
		if child := findFirst(n, func(x *html.Node) bool { return x.DataAtom == atom.Code }); child != nil {
			lang = codeLanguage(child)
		}
	}
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + code + "\n" + fence
}

func preText(n *html.Node, b *strings.Builder) {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		switch {
		case ch.Type == html.TextNode:
			b.WriteString(ch.Data)
		case ch.DataAtom == atom.Br:
			b.WriteString("\n")
		case ch.Type == html.ElementNode && (ch.DataAtom == atom.Script || ch.DataAtom == atom.Style):
		default:
			preText(ch, b)
		}
	}
}

var langClass = regexp.MustCompile(`(?:^|\s)(?:language|lang|highlight-source|brush:?)-?([A-Za-z0-9_+#.-]+)`)

func codeLanguage(n *html.Node) string {
	if l := attr(n, "data-lang"); l != "" {
		return strings.Fields(l)[0]
	}
	if m := langClass.FindStringSubmatch(attr(n, "class")); m != nil {
		return m[1]
	}
	return ""
}

// list renders ul/ol, indenting continuation lines and nested lists under
// their item marker.
func (c *converter) list(n *html.Node) string {
	ordered := n.DataAtom == atom.Ol
	idx := 1
	if s, err := strconv.Atoi(attr(n, "start")); err == nil && ordered {
		idx = s
	}
	var items []string
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if dropped(li) {
			continue
		}
		var parts []string
		if li.Type == html.ElementNode && li.DataAtom == atom.Li {
			parts = c.blocks(li)
		} else if li.Type == html.ElementNode && (li.DataAtom == atom.Ul || li.DataAtom == atom.Ol) {
			// A list nested directly in a list (invalid but common).
			if len(items) > 0 {
				if sub := c.list(li); sub != "" {
					items[len(items)-1] += "\n" + indent(sub, "  ")
				}
			}
			continue
		} else {
			if t := cleanInline(c.inline(li)); t != "" {
				parts = []string{t}
			} else {
				continue
			}
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(idx) + ". "
			idx++
		}
		body := strings.Join(parts, "\n")
		if strings.TrimSpace(body) == "" {
			continue
		}
		pad := strings.Repeat(" ", len(marker))
		lines := strings.Split(body, "\n")
		for i := range lines {
			if i == 0 {
				lines[i] = marker + lines[i]
			} else if lines[i] != "" {
				lines[i] = pad + lines[i]
			}
		}
		items = append(items, strings.Join(lines, "\n"))
	}
	return strings.Join(items, "\n")
}

func (c *converter) definitionList(n *html.Node) []string {
	var out []string
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if dropped(ch) || ch.Type != html.ElementNode {
			continue
		}
		t := cleanInline(c.inlineChildren(ch))
		if t == "" {
			continue
		}
		switch ch.DataAtom {
		case atom.Dt:
			out = append(out, "**"+strings.ReplaceAll(t, "\n", " ")+"**")
		case atom.Dd:
			if len(out) > 0 {
				out[len(out)-1] += "\n: " + strings.ReplaceAll(t, "\n", "\n  ")
			} else {
				out = append(out, ": "+t)
			}
		default:
			out = append(out, c.block(ch)...)
		}
	}
	return out
}

// table renders a GitHub-flavoured Markdown table. The first row (from
// thead, or the first row with th cells) is the header; colspan repeats
// empty cells so columns stay aligned.
func (c *converter) table(n *html.Node) string {
	var rows [][]string
	headerRows := 0
	var visit func(x *html.Node, inHead bool)
	visit = func(x *html.Node, inHead bool) {
		for ch := x.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode || dropped(ch) {
				continue
			}
			switch ch.DataAtom {
			case atom.Thead:
				visit(ch, true)
			case atom.Tbody, atom.Tfoot:
				visit(ch, false)
			case atom.Tr:
				var row []string
				allTh := true
				for cell := ch.FirstChild; cell != nil; cell = cell.NextSibling {
					if cell.Type != html.ElementNode || (cell.DataAtom != atom.Td && cell.DataAtom != atom.Th) {
						continue
					}
					if cell.DataAtom == atom.Td {
						allTh = false
					}
					text := strings.Join(c.blocks(cell), " ")
					text = strings.TrimSpace(spaceRun.ReplaceAllString(text, " "))
					row = append(row, strings.ReplaceAll(text, "|", `\|`))
					if span, err := strconv.Atoi(attr(cell, "colspan")); err == nil && span > 1 {
						for k := 1; k < span && k < 64; k++ {
							row = append(row, "")
						}
					}
				}
				if len(row) == 0 {
					continue
				}
				if (inHead || (allTh && len(rows) == 0)) && len(rows) == headerRows {
					headerRows++
				}
				rows = append(rows, row)
			}
		}
	}
	visit(n, false)
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	var caption string
	if capNode := findFirst(n, func(x *html.Node) bool { return x.DataAtom == atom.Caption }); capNode != nil {
		caption = cleanInline(c.inlineChildren(capNode))
	}
	// A one-column table is a layout table: render its cells as paragraphs.
	if cols == 1 {
		var parts []string
		for _, r := range rows {
			if r[0] != "" {
				parts = append(parts, r[0])
			}
		}
		return strings.Join(parts, "\n\n")
	}
	header := make([]string, cols)
	body := rows
	if headerRows > 0 {
		copy(header, rows[0])
		body = rows[1:]
	}
	var b strings.Builder
	if caption != "" {
		b.WriteString("**" + caption + "**\n\n")
	}
	writeRow := func(r []string) {
		b.WriteString("|")
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			b.WriteString(" " + cell + " |")
		}
		b.WriteString("\n")
	}
	writeRow(header)
	b.WriteString("|" + strings.Repeat(" --- |", cols) + "\n")
	for _, r := range body {
		writeRow(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (c *converter) resolve(ref string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil, err
	}
	if c.base != nil {
		u = c.base.ResolveReference(u)
	}
	return u, nil
}

// linkURL renders a URL for a Markdown link, escaping characters that would
// end the link early.
func linkURL(u *url.URL) string {
	s := u.String()
	s = strings.ReplaceAll(s, " ", "%20")
	s = strings.ReplaceAll(s, "(", "%28")
	return strings.ReplaceAll(s, ")", "%29")
}

func escapeBrackets(s string) string {
	s = strings.ReplaceAll(s, "[", `\[`)
	return strings.ReplaceAll(s, "]", `\]`)
}

var (
	spaceAroundNL = regexp.MustCompile(` *\n *`)
	multiSpace    = regexp.MustCompile(` {2,}`)
	manyNL        = regexp.MustCompile(`\n{3,}`)
)

// cleanInline normalises inline text: single spaces, no spaces around line
// breaks, no leading or trailing blank space.
func cleanInline(s string) string {
	s = multiSpace.ReplaceAllString(s, " ")
	s = spaceAroundNL.ReplaceAllString(s, "\n")
	s = manyNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// tidy removes trailing spaces and runs of blank lines outside code fences.
func tidy(md string) string {
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	fence := ""
	blank := 0
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if fence == "" && strings.HasPrefix(trimmed, "```") {
			fence = trimmed[:len(trimmed)-len(strings.TrimLeft(trimmed, "`"))]
		} else if fence != "" && trimmed == fence {
			fence = ""
			out = append(out, l)
			blank = 0
			continue
		}
		if fence != "" {
			out = append(out, l)
			blank = 0
			continue
		}
		l = strings.TrimRight(l, " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func collapseSpace(s string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
}

func textContent(n *html.Node) string {
	var b strings.Builder
	walk(n, func(x *html.Node) bool {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		return !(x.Type == html.ElementNode && (x.DataAtom == atom.Script || x.DataAtom == atom.Style))
	})
	return b.String()
}

// walk visits n and its descendants depth-first; f returns false to skip a
// node's children.
func walk(n *html.Node, f func(*html.Node) bool) {
	if !f(n) {
		return
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		walk(ch, f)
	}
}

func findFirst(n *html.Node, match func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(n, func(x *html.Node) bool {
		if found != nil {
			return false
		}
		if x.Type == html.ElementNode && match(x) {
			found = x
			return false
		}
		return true
	})
	return found
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}
