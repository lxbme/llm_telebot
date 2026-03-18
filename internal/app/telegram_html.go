package app

import (
	"bytes"
	"fmt"
	"html"
	"log"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
	tele "gopkg.in/telebot.v3"
)

// ─── Markdown → Telegram HTML ─────────────────────────────────────────────────

// mdToTgHTML converts Markdown text (as produced by LLMs) into
// Telegram-compatible HTML.  It uses goldmark to parse the Markdown AST
// and a custom renderer that emits only the HTML subset Telegram supports:
//
//	<b>, <i>, <s>, <u>, <code>, <pre>, <a>, <blockquote>
func mdToTgHTML(markdown string) string {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Strikethrough),
		goldmark.WithRenderer(
			renderer.NewRenderer(
				renderer.WithNodeRenderers(
					util.Prioritized(&tgHTMLRenderer{}, 1000),
				),
			),
		),
	)

	var buf bytes.Buffer
	if err := md.Convert([]byte(markdown), &buf); err != nil {
		// If parsing fails, return HTML-escaped plain text as a safe fallback.
		return html.EscapeString(markdown)
	}
	return strings.TrimRight(buf.String(), "\n ")
}

// ─── Custom Goldmark Renderer ─────────────────────────────────────────────────

type listInfo struct {
	ordered bool
	index   int
}

type tgHTMLRenderer struct {
	listStack []listInfo
}

func (r *tgHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	// Block nodes
	reg.Register(ast.KindDocument, r.renderDocument)
	reg.Register(ast.KindHeading, r.renderHeading)
	reg.Register(ast.KindBlockquote, r.renderBlockquote)
	reg.Register(ast.KindCodeBlock, r.renderCodeBlock)
	reg.Register(ast.KindFencedCodeBlock, r.renderFencedCodeBlock)
	reg.Register(ast.KindHTMLBlock, r.renderHTMLBlock)
	reg.Register(ast.KindList, r.renderList)
	reg.Register(ast.KindListItem, r.renderListItem)
	reg.Register(ast.KindParagraph, r.renderParagraph)
	reg.Register(ast.KindTextBlock, r.renderTextBlock)
	reg.Register(ast.KindThematicBreak, r.renderThematicBreak)

	// Inline nodes
	reg.Register(ast.KindAutoLink, r.renderAutoLink)
	reg.Register(ast.KindCodeSpan, r.renderCodeSpan)
	reg.Register(ast.KindEmphasis, r.renderEmphasis)
	reg.Register(ast.KindImage, r.renderImage)
	reg.Register(ast.KindLink, r.renderLink)
	reg.Register(ast.KindRawHTML, r.renderRawHTML)
	reg.Register(ast.KindText, r.renderText)
	reg.Register(ast.KindString, r.renderString)

	// Extension nodes
	reg.Register(east.KindStrikethrough, r.renderStrikethrough)
}

// ── Block Renderers ───────────────────────────────────────────────────────────

func (r *tgHTMLRenderer) renderDocument(w util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderHeading(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("<b>")
	} else {
		w.WriteString("</b>\n\n")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderBlockquote(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("<blockquote>")
	} else {
		w.WriteString("</blockquote>\n")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderCodeBlock(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("<pre><code>")
		r.writeLines(w, src, node)
		w.WriteString("</code></pre>\n")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderFencedCodeBlock(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.FencedCodeBlock)
		if lang := n.Language(src); lang != nil {
			fmt.Fprintf(w, "<pre><code class=\"language-%s\">", html.EscapeString(string(lang)))
		} else {
			w.WriteString("<pre><code>")
		}
		r.writeLines(w, src, node)
		w.WriteString("</code></pre>\n")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderHTMLBlock(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		// Escape raw HTML blocks — they may contain tags unsupported by Telegram.
		for i := 0; i < node.Lines().Len(); i++ {
			line := node.Lines().At(i)
			w.WriteString(html.EscapeString(string(line.Value(src))))
		}
		w.WriteByte('\n')
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderList(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.List)
	if entering {
		r.listStack = append(r.listStack, listInfo{ordered: n.IsOrdered(), index: n.Start})
	} else {
		if len(r.listStack) > 0 {
			r.listStack = r.listStack[:len(r.listStack)-1]
		}
		// Add blank line after top-level lists.
		if !isInList(node) {
			w.WriteByte('\n')
		}
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderListItem(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering && len(r.listStack) > 0 {
		top := &r.listStack[len(r.listStack)-1]
		indent := strings.Repeat("  ", len(r.listStack)-1)
		if top.ordered {
			fmt.Fprintf(w, "%s%d. ", indent, top.index)
			top.index++
		} else {
			fmt.Fprintf(w, "%s• ", indent)
		}
	}
	if !entering {
		// Add a newline after the list item, but only if the last child
		// isn't a nested list (which already ends with its own newlines).
		last := node.LastChild()
		if last != nil && last.Kind() == ast.KindList {
			// Nested list already wrote its trailing newline.
			return ast.WalkContinue, nil
		}
		w.WriteByte('\n')
		// For loose lists (items contain Paragraph nodes), add an extra
		// blank line between items for visual spacing — but not after
		// the last item.
		if node.NextSibling() != nil && isLooseListItem(node) {
			w.WriteByte('\n')
		}
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderParagraph(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		if isInListItem(node) {
			// In a loose list (blank lines between items), paragraphs
			// need a trailing newline for proper spacing between items.
			// In tight lists, TextBlock is used instead of Paragraph.
			if node.NextSibling() != nil {
				// Multi-paragraph list item: separate paragraphs.
				w.WriteByte('\n')
			}
		} else if isInBlockquote(node) {
			w.WriteByte('\n')
		} else {
			w.WriteString("\n\n")
		}
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderTextBlock(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering && node.NextSibling() != nil {
		// TextBlock is used in tight lists. When followed by a sibling
		// (e.g. a nested list), add a newline to separate them.
		w.WriteByte('\n')
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderThematicBreak(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("———\n\n")
	}
	return ast.WalkContinue, nil
}

// ── Inline Renderers ──────────────────────────────────────────────────────────

func (r *tgHTMLRenderer) renderAutoLink(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.AutoLink)
		url := n.URL(src)
		label := n.Label(src)
		fmt.Fprintf(w, "<a href=\"%s\">%s</a>",
			html.EscapeString(string(url)),
			html.EscapeString(string(label)))
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderCodeSpan(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("<code>")
	} else {
		w.WriteString("</code>")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderEmphasis(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.Emphasis)
	tag := "i"
	if n.Level == 2 {
		tag = "b"
	}
	if entering {
		fmt.Fprintf(w, "<%s>", tag)
	} else {
		fmt.Fprintf(w, "</%s>", tag)
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderImage(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.Image)
		fmt.Fprintf(w, "<a href=\"%s\">", html.EscapeString(string(n.Destination)))
	} else {
		w.WriteString("</a>")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderLink(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.Link)
		fmt.Fprintf(w, "<a href=\"%s\">", html.EscapeString(string(n.Destination)))
	} else {
		w.WriteString("</a>")
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderRawHTML(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		// Escape inline raw HTML — it may contain tags unsupported by Telegram.
		n := node.(*ast.RawHTML)
		for i := 0; i < n.Segments.Len(); i++ {
			seg := n.Segments.At(i)
			w.WriteString(html.EscapeString(string(seg.Value(src))))
		}
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderText(w util.BufWriter, src []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.Text)
		w.WriteString(html.EscapeString(string(n.Segment.Value(src))))
		if n.HardLineBreak() || n.SoftLineBreak() {
			w.WriteByte('\n')
		}
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderString(w util.BufWriter, _ []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		n := node.(*ast.String)
		if n.IsCode() {
			w.Write(n.Value)
		} else {
			w.WriteString(html.EscapeString(string(n.Value)))
		}
	}
	return ast.WalkContinue, nil
}

func (r *tgHTMLRenderer) renderStrikethrough(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("<s>")
	} else {
		w.WriteString("</s>")
	}
	return ast.WalkContinue, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (r *tgHTMLRenderer) writeLines(w util.BufWriter, src []byte, node ast.Node) {
	for i := 0; i < node.Lines().Len(); i++ {
		line := node.Lines().At(i)
		w.WriteString(html.EscapeString(string(line.Value(src))))
	}
}

func isInListItem(node ast.Node) bool {
	for p := node.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindListItem {
			return true
		}
	}
	return false
}

func isInList(node ast.Node) bool {
	for p := node.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindList {
			return true
		}
	}
	return false
}

func isInBlockquote(node ast.Node) bool {
	for p := node.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindBlockquote {
			return true
		}
	}
	return false
}

// isLooseListItem returns true if a ListItem node belongs to a "loose" list
// (one where items are separated by blank lines in Markdown). In goldmark's
// AST, loose list items contain Paragraph children, while tight list items
// contain TextBlock children.
func isLooseListItem(node ast.Node) bool {
	for c := node.FirstChild(); c != nil; c = c.NextSibling() {
		if c.Kind() == ast.KindParagraph {
			return true
		}
	}
	return false
}

// ─── Bot Integration ──────────────────────────────────────────────────────────

// editFinalResponse edits the placeholder with the LLM's final Markdown
// response, converting it to Telegram HTML.  If the HTML exceeds Telegram's
// 4096-char limit or the API rejects it, it falls back to plain text.
func (b *Bot) editFinalResponse(msg *tele.Message, markdownText string) {
	htmlText := mdToTgHTML(markdownText)
	if len([]rune(htmlText)) <= 4096 {
		_, err := b.tg.Edit(msg, htmlText, tele.ModeHTML)
		if err == nil {
			return
		}
		if strings.Contains(err.Error(), "message is not modified") {
			return
		}
		// HTML edit failed (e.g., malformed tags); fall back to plain text.
		log.Printf("HTML edit failed, falling back to plain text: %v", err)
	}
	b.editOrLog(msg, markdownText)
}
