package wp2astro

import (
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTML to Markdown for the WordPress to Astro converter.
//
// The rule this file follows: content is never dropped. Anything that converts
// cleanly (paragraphs, headings, lists, quotes, code, images, links, emphasis)
// becomes Markdown. Anything that does not (tables, embeds, forms, galleries,
// page-builder layouts, inline styles) is kept as raw HTML, which Astro's
// Markdown renders as-is. A plainer post beats a shorter one.

// mdRewrite resolves a URL found in the content. kind is "img" for an image
// source and "link" for anything else. It returns the URL to write.
type mdRewrite func(kind, raw string) string

// mdResult is one converted document.
type mdResult struct {
	Markdown string
	// RawBlocks counts the blocks kept as raw HTML.
	RawBlocks int
	// Forms counts <form> elements (they post to WordPress and stop working).
	Forms int
	// Scripts counts <script> elements kept in raw blocks.
	Scripts int
	// Shortcodes lists unrendered [shortcode] names found in the text.
	Shortcodes []string
}

type mdConverter struct {
	rewrite    mdRewrite
	raw        int
	forms      int
	scripts    int
	shortcodes map[string]bool
}

// htmlToMarkdown converts a WordPress `content.rendered` fragment.
func htmlToMarkdown(src string, rewrite mdRewrite) mdResult {
	if rewrite == nil {
		rewrite = func(_, raw string) string { return raw }
	}
	c := &mdConverter{rewrite: rewrite, shortcodes: map[string]bool{}}
	body := &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"}
	nodes, err := html.ParseFragment(strings.NewReader(src), body)
	if err != nil {
		// The tokenizer accepts anything; this is unreachable in practice. Keep
		// the original so nothing is lost.
		return mdResult{Markdown: strings.TrimSpace(src), RawBlocks: 1}
	}
	out := c.blocks(nodes)
	res := mdResult{
		Markdown:  strings.TrimSpace(out) + "\n",
		RawBlocks: c.raw,
		Forms:     c.forms,
		Scripts:   c.scripts,
	}
	for name := range c.shortcodes {
		res.Shortcodes = append(res.Shortcodes, name)
	}
	sort.Strings(res.Shortcodes)
	return res
}

// ------------------------------------------------------------- block level

var blockUnwrap = map[atom.Atom]bool{
	atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true,
	atom.Header: true, atom.Footer: true, atom.Aside: true, atom.Center: true,
}

var inlineAtoms = map[atom.Atom]bool{
	atom.A: true, atom.Strong: true, atom.B: true, atom.Em: true, atom.I: true,
	atom.Code: true, atom.Br: true, atom.Img: true, atom.Span: true, atom.Del: true,
	atom.S: true, atom.Strike: true, atom.Sup: true, atom.Sub: true, atom.Mark: true,
	atom.Abbr: true, atom.Kbd: true, atom.Small: true, atom.U: true, atom.Q: true,
	atom.Cite: true, atom.Time: true, atom.Font: true, atom.Ins: true, atom.Var: true,
	atom.Samp: true, atom.Dfn: true, atom.Bdi: true, atom.Bdo: true, atom.Wbr: true,
	atom.Data: true, atom.Label: true,
}

// blocks converts a sequence of sibling nodes. Consecutive inline nodes and
// text are gathered into one paragraph.
func (c *mdConverter) blocks(nodes []*html.Node) string {
	var parts []string
	var inline []*html.Node
	flush := func() {
		if len(inline) == 0 {
			return
		}
		if p := c.paragraph(inline); p != "" {
			parts = append(parts, p)
		}
		inline = nil
	}
	for _, n := range nodes {
		switch {
		case n.Type == html.CommentNode, n.Type == html.DoctypeNode:
			continue
		case n.Type == html.TextNode, n.Type == html.ElementNode && inlineAtoms[n.DataAtom] && !hasBlockDescendant(n):
			inline = append(inline, n)
		case n.Type == html.ElementNode:
			flush()
			if b := c.block(n); b != "" {
				parts = append(parts, b)
			}
		}
	}
	flush()
	return strings.Join(parts, "\n\n")
}

func (c *mdConverter) block(n *html.Node) string {
	switch n.DataAtom {
	case atom.P:
		if !c.inlineClean(n) {
			return c.rawBlock(n)
		}
		return c.paragraph(childNodes(n))
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		// A heading with an id is an anchor target (tables of contents link to
		// it). Markdown would give it a different generated id, so keep it.
		if attr(n, "id") != "" || !c.inlineClean(n) {
			return c.rawBlock(n)
		}
		text := strings.TrimSpace(collapseNewlines(c.inlines(childNodes(n))))
		if text == "" {
			return ""
		}
		level := int(n.Data[1] - '0')
		return strings.Repeat("#", level) + " " + text
	case atom.Hr:
		return "---"
	case atom.Ul, atom.Ol:
		if !c.listClean(n) {
			return c.rawBlock(n)
		}
		return c.list(n, 0)
	case atom.Blockquote:
		if !c.flowClean(n) {
			return c.rawBlock(n)
		}
		inner := strings.TrimSpace(c.blocks(childNodes(n)))
		if inner == "" {
			return ""
		}
		return prefixLines(inner, "> ", "> ")
	case atom.Pre:
		return c.pre(n)
	case atom.Figure:
		if md, ok := c.figure(n); ok {
			return md
		}
		return c.rawBlock(n)
	case atom.Img:
		return c.inlines([]*html.Node{n})
	case atom.Script, atom.Style, atom.Noscript:
		return c.rawBlock(n)
	}
	if blockUnwrap[n.DataAtom] && attr(n, "id") == "" && (attr(n, "style") == "" || layoutWrapper(n)) {
		return c.blocks(childNodes(n))
	}
	return c.rawBlock(n)
}

func (c *mdConverter) paragraph(nodes []*html.Node) string {
	text := strings.TrimSpace(c.inlines(nodes))
	if text == "" {
		return ""
	}
	return escapeLineStarts(text)
}

// list renders ul/ol. depth only affects indentation of nested lists.
func (c *mdConverter) list(n *html.Node, depth int) string {
	ordered := n.DataAtom == atom.Ol
	num := 1
	if ordered {
		if s := attr(n, "start"); s != "" {
			if v, ok := atoiSmall(s); ok {
				num = v
			}
		}
	}
	var items []string
	for _, li := range childNodes(n) {
		if li.Type != html.ElementNode || li.DataAtom != atom.Li {
			continue
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		var parts []string
		var inline []*html.Node
		flush := func() {
			if len(inline) > 0 {
				if t := strings.TrimSpace(c.inlines(inline)); t != "" {
					parts = append(parts, escapeLineStarts(t))
				}
				inline = nil
			}
		}
		for _, ch := range childNodes(li) {
			switch {
			case ch.Type == html.ElementNode && (ch.DataAtom == atom.Ul || ch.DataAtom == atom.Ol):
				flush()
				parts = append(parts, c.list(ch, depth+1))
			case ch.Type == html.ElementNode && !inlineAtoms[ch.DataAtom]:
				flush()
				if b := c.block(ch); b != "" {
					parts = append(parts, b)
				}
			case ch.Type == html.TextNode, ch.Type == html.ElementNode:
				inline = append(inline, ch)
			}
		}
		flush()
		body := strings.Join(parts, "\n")
		if body == "" {
			body = " "
		}
		indent := strings.Repeat(" ", len(marker))
		items = append(items, prefixLines(body, marker, indent))
	}
	return strings.Join(items, "\n")
}

func (c *mdConverter) pre(n *html.Node) string {
	lang := ""
	var code *html.Node
	kids := elementChildren(n)
	if len(kids) == 1 && kids[0].DataAtom == atom.Code {
		code = kids[0]
	}
	for _, src := range []*html.Node{code, n} {
		if src == nil {
			continue
		}
		for _, cls := range strings.Fields(attr(src, "class")) {
			if strings.HasPrefix(cls, "language-") {
				lang = strings.TrimPrefix(cls, "language-")
			} else if strings.HasPrefix(cls, "lang-") {
				lang = strings.TrimPrefix(cls, "lang-")
			}
		}
		if lang != "" {
			break
		}
	}
	text := strings.Trim(textContent(n), "\n")
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + text + "\n" + fence
}

// figure handles the common WordPress image block: an image (optionally
// linked) plus an optional caption. Anything else in a figure stays raw.
func (c *mdConverter) figure(n *html.Node) (string, bool) {
	var media []*html.Node
	var caption *html.Node
	for _, ch := range childNodes(n) {
		switch {
		case ch.Type == html.TextNode && strings.TrimSpace(ch.Data) == "":
		case ch.Type == html.ElementNode && ch.DataAtom == atom.Figcaption:
			caption = ch
		case ch.Type == html.ElementNode && ch.DataAtom == atom.Img:
			media = append(media, ch)
		case ch.Type == html.ElementNode && ch.DataAtom == atom.A && onlyImages(ch):
			media = append(media, ch)
		default:
			return "", false
		}
	}
	if len(media) != 1 {
		return "", false
	}
	if caption != nil && !c.inlineClean(caption) {
		return "", false
	}
	out := c.inlines(media)
	if caption != nil {
		if t := strings.TrimSpace(c.inlines(childNodes(caption))); t != "" {
			out += "\n\n*" + strings.TrimSpace(t) + "*"
		}
	}
	return out, true
}

// layoutWrapper reports whether n is a Gutenberg layout container (columns,
// groups) whose inline style only positions its children. Without the theme's
// CSS the layout is gone anyway; unwrapping lets the content inside convert.
func layoutWrapper(n *html.Node) bool {
	for _, cls := range strings.Fields(attr(n, "class")) {
		switch cls {
		case "wp-block-columns", "wp-block-column", "wp-block-group", "wp-block-group__inner-container",
			"wp-block-cover__inner-container", "wp-block-media-text__content":
			return true
		}
	}
	return false
}

// rawBlock keeps n as HTML. URLs inside are still rewritten so downloaded
// images and internal links resolve. Blank lines are removed: in Markdown a
// blank line ends an HTML block, and whatever followed would be re-read as
// Markdown (an indented line becomes a code block).
func (c *mdConverter) rawBlock(n *html.Node) string {
	c.raw++
	c.rewriteTree(n)
	var buf bytes.Buffer
	if err := html.Render(&buf, n); err != nil {
		return ""
	}
	s := buf.String()
	switch n.DataAtom {
	case atom.Pre, atom.Script, atom.Style, atom.Textarea:
		// CommonMark lets these contain blank lines.
		return s
	}
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// rewriteTree rewrites URL attributes in a subtree we are about to keep as
// raw HTML, and counts the things that will not work on a static site.
func (c *mdConverter) rewriteTree(n *html.Node) {
	if n.Type == html.ElementNode {
		switch n.DataAtom {
		case atom.Form:
			c.forms++
		case atom.Script:
			c.scripts++
		}
		if n.DataAtom == atom.Img {
			promoteLazySrc(n)
			// We download one size of each image. srcset would point the browser
			// at sizes we did not save.
			removeAttr(n, "srcset")
			removeAttr(n, "sizes")
		}
		for i, a := range n.Attr {
			switch a.Key {
			case "src", "poster":
				// Only an <img> src (and a video poster) is an image we
				// download; an iframe, script or video src is a link.
				kind := "link"
				if n.DataAtom == atom.Img || a.Key == "poster" {
					kind = "img"
				}
				n.Attr[i].Val = c.rewrite(kind, a.Val)
			case "href":
				if n.DataAtom == atom.Link {
					continue
				}
				n.Attr[i].Val = c.rewrite("link", a.Val)
			}
		}
	}
	if n.Type == html.TextNode {
		c.noteShortcodes(n.Data)
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.rewriteTree(ch)
	}
}

// --------------------------------------------------------------- inline level

func (c *mdConverter) inlines(nodes []*html.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		c.inline(&b, n)
	}
	return b.String()
}

func (c *mdConverter) inline(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		c.noteShortcodes(n.Data)
		t := escapeMarkdownText(collapseSpace(n.Data))
		// "Wow!" followed by a link would read as an image: ![text](url).
		if strings.HasSuffix(t, "!") && n.NextSibling != nil {
			t = t[:len(t)-1] + `\!`
		}
		b.WriteString(t)
		return
	case html.ElementNode:
	default:
		return
	}
	switch n.DataAtom {
	case atom.Br:
		b.WriteString("\\\n")
	case atom.Strong, atom.B:
		wrapInline(b, "**", c.inlines(childNodes(n)))
	case atom.Em, atom.I:
		wrapInline(b, "*", c.inlines(childNodes(n)))
	case atom.Del, atom.S, atom.Strike:
		wrapInline(b, "~~", c.inlines(childNodes(n)))
	case atom.Code:
		t := textContent(n)
		if t == "" {
			return
		}
		tick := "`"
		for strings.Contains(t, tick) {
			tick += "`"
		}
		pad := ""
		if strings.HasPrefix(t, "`") || strings.HasSuffix(t, "`") {
			pad = " "
		}
		b.WriteString(tick + pad + t + pad + tick)
	case atom.A:
		href := strings.TrimSpace(attr(n, "href"))
		text := strings.TrimSpace(c.inlines(childNodes(n)))
		if href == "" || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			b.WriteString(text)
			return
		}
		if text == "" {
			text = escapeMarkdownText(href)
		}
		b.WriteString("[" + text + "](" + mdURL(c.rewrite("link", href)) + mdTitle(attr(n, "title")) + ")")
	case atom.Img:
		promoteLazySrc(n)
		src := strings.TrimSpace(attr(n, "src"))
		if src == "" {
			return
		}
		alt := strings.NewReplacer("[", "\\[", "]", "\\]", "\n", " ").Replace(attr(n, "alt"))
		b.WriteString("![" + alt + "](" + mdURL(c.rewrite("img", src)) + mdTitle(attr(n, "title")) + ")")
	case atom.Span, atom.Font, atom.Label, atom.Data, atom.Time, atom.Bdi:
		if n.DataAtom == atom.Span && attr(n, "style") == "" && attr(n, "id") == "" {
			b.WriteString(c.inlines(childNodes(n)))
			return
		}
		if n.DataAtom == atom.Time || n.DataAtom == atom.Data || n.DataAtom == atom.Label || n.DataAtom == atom.Bdi {
			b.WriteString(c.inlines(childNodes(n)))
			return
		}
		b.WriteString(c.rawInline(n))
	default:
		// sup, sub, mark, abbr, kbd, u, q, cite, ...: Markdown has no syntax
		// for them, and inline HTML is valid Markdown.
		b.WriteString(c.rawInline(n))
	}
}

func (c *mdConverter) rawInline(n *html.Node) string {
	c.rewriteTree(n)
	var buf bytes.Buffer
	if err := html.Render(&buf, n); err != nil {
		return ""
	}
	return collapseNewlines(buf.String())
}

func wrapInline(b *strings.Builder, mark, inner string) {
	t := strings.TrimSpace(inner)
	if t == "" {
		b.WriteString(inner)
		return
	}
	// Markdown emphasis cannot start or end on whitespace; move it outside.
	lead := inner[:len(inner)-len(strings.TrimLeft(inner, " "))]
	trail := inner[len(strings.TrimRight(inner, " ")):]
	b.WriteString(lead + mark + t + mark + trail)
}

// ---------------------------------------------------------- clean-ness checks

// inlineClean reports whether every descendant of n converts to Markdown
// without losing anything that matters.
func (c *mdConverter) inlineClean(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		switch ch.Type {
		case html.TextNode, html.CommentNode:
			continue
		case html.ElementNode:
			if !inlineAtoms[ch.DataAtom] {
				return false
			}
			if !c.inlineClean(ch) {
				return false
			}
		}
	}
	return true
}

// flowClean is for containers (blockquote, li) whose children may be blocks.
func (c *mdConverter) flowClean(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode {
			continue
		}
		switch {
		case inlineAtoms[ch.DataAtom]:
			if !c.inlineClean(ch) {
				return false
			}
		case ch.DataAtom == atom.P:
			if !c.inlineClean(ch) {
				return false
			}
		case ch.DataAtom == atom.Ul || ch.DataAtom == atom.Ol:
			if !c.listClean(ch) {
				return false
			}
		case ch.DataAtom == atom.Pre, ch.DataAtom == atom.Hr:
		case ch.DataAtom == atom.Blockquote:
			if !c.flowClean(ch) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (c *mdConverter) listClean(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type != html.ElementNode {
			continue
		}
		if ch.DataAtom != atom.Li || attr(ch, "style") != "" || !c.flowClean(ch) {
			return false
		}
	}
	return true
}

func hasBlockDescendant(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode && (!inlineAtoms[ch.DataAtom] || hasBlockDescendant(ch)) {
			return true
		}
	}
	return false
}

func onlyImages(n *html.Node) bool {
	found := false
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		switch {
		case ch.Type == html.TextNode && strings.TrimSpace(ch.Data) == "":
		case ch.Type == html.ElementNode && ch.DataAtom == atom.Img:
			found = true
		default:
			return false
		}
	}
	return found
}

// ------------------------------------------------------------------ helpers

// shortcodeRe matches an unrendered WordPress shortcode: `[gallery ids="1"]`,
// `[contact-form-7 id="5"]`, `[/caption]`. It requires either attributes or a
// name with a separator so a footnote like [1] or a bracketed word is not
// mistaken for one.
var shortcodeRe = regexp.MustCompile(`\[(/?)([a-z][a-z0-9]*(?:[_-][a-z0-9]+)*)(\s+[a-z_-]+=[^\]]*)?\]`)

var knownShortcodes = map[string]bool{
	"gallery": true, "caption": true, "audio": true, "video": true, "playlist": true,
	"embed": true, "wp_caption": true, "contact-form-7": true, "contact-form": true,
	"wpforms": true, "gravityform": true, "su_button": true, "vc_row": true,
	"vc_column": true, "et_pb_section": true, "fusion_builder_container": true,
}

func (c *mdConverter) noteShortcodes(text string) {
	for _, m := range shortcodeRe.FindAllStringSubmatch(text, -1) {
		name := m[2]
		if m[3] != "" || m[1] == "/" || strings.ContainsAny(name, "_-") || knownShortcodes[name] {
			c.shortcodes[name] = true
		}
	}
}

func promoteLazySrc(n *html.Node) {
	for _, k := range []string{"data-src", "data-lazy-src", "data-orig-file"} {
		if v := strings.TrimSpace(attr(n, k)); v != "" {
			setAttr(n, "src", v)
			return
		}
	}
}

func childNodes(n *html.Node) []*html.Node {
	var out []*html.Node
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		out = append(out, ch)
	}
	return out
}

func elementChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode {
			out = append(out, ch)
		} else if ch.Type == html.TextNode && strings.TrimSpace(ch.Data) != "" {
			return nil
		}
	}
	return out
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i, a := range n.Attr {
		if a.Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func removeAttr(n *html.Node, key string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			out = append(out, a)
		}
	}
	n.Attr = out
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Br {
			b.WriteString("\n")
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

var spaceRe = regexp.MustCompile(`[ \t\r\n\f]+`)

func collapseSpace(s string) string { return spaceRe.ReplaceAllString(s, " ") }

func collapseNewlines(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\\\n", " ")), " ")
}

var mdEscaper = strings.NewReplacer(
	`\`, `\\`, "`", "\\`", "*", `\*`, "_", `\_`, "[", `\[`, "]", `\]`, "<", "&lt;", "~", `\~`,
)

var entityLikeRe = regexp.MustCompile(`&([a-zA-Z][a-zA-Z0-9]*|#[0-9]+|#[xX][0-9a-fA-F]+);`)

func escapeMarkdownText(s string) string {
	// Text like "&amp;copy;" was decoded to "&copy;"; written back verbatim it
	// would render as a symbol. Re-escape anything that looks like an entity,
	// before the escaper below adds entities of its own.
	s = entityLikeRe.ReplaceAllString(s, "&amp;$1;")
	return mdEscaper.Replace(s)
}

var (
	orderedStartRe = regexp.MustCompile(`^(\s*\d+)([.)]\s)`)
	blockStartRe   = regexp.MustCompile(`^(\s*)(#{1,6}(\s|$)|>|[-+*]\s|[-=]{3,}\s*$|~{3,})`)
)

// escapeLineStarts stops a paragraph line from being read as a heading, quote,
// list item or rule.
func escapeLineStarts(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if m := orderedStartRe.FindStringSubmatchIndex(l); m != nil {
			lines[i] = l[:m[3]] + `\` + l[m[3]:]
		} else if m := blockStartRe.FindStringSubmatchIndex(l); m != nil {
			lines[i] = l[:m[3]] + `\` + l[m[3]:]
		}
	}
	return strings.Join(lines, "\n")
}

func prefixLines(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		if strings.TrimSpace(l) == "" {
			lines[i] = strings.TrimRight(p, " ")
			continue
		}
		lines[i] = p + l
	}
	return strings.Join(lines, "\n")
}

func mdURL(u string) string {
	if strings.ContainsAny(u, " ()<>") {
		return "<" + strings.NewReplacer("<", "%3C", ">", "%3E").Replace(u) + ">"
	}
	return u
}

func mdTitle(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	return ` "` + strings.ReplaceAll(t, `"`, `\"`) + `"`
}

func atoiSmall(s string) (int, bool) {
	if s == "" || len(s) > 6 {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}
