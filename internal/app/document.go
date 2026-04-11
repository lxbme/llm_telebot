package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
)

// defaultDocumentAllowedExts is the default whitelist of file extensions
// accepted by handleDocument. Extensions with a registered parser
// (see documentParsers) get specialized handling; everything else falls
// through to the plain UTF-8 reader.
const defaultDocumentAllowedExts = "txt,md,markdown,csv,tsv,json,xml,yaml,yml,toml,ini,log,pdf," +
	"docx,odt,pptx,epub,xlsx,xlsm," +
	"go,py,js,ts,tsx,jsx,rs,java,c,cc,cpp,h,hpp,sh,rb,php,kt,swift,html,css,sql"

// documentExtAllowed reports whether ext (without leading dot, any case)
// appears in the comma-separated whitelist. An empty ext always fails.
func documentExtAllowed(whitelist, ext string) bool {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	if ext == "" {
		return false
	}
	for entry := range strings.SplitSeq(whitelist, ",") {
		entry = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(entry), "."))
		if entry != "" && entry == ext {
			return true
		}
	}
	return false
}

// documentParser turns a raw document body into UTF-8 text. Parsers MUST NOT
// apply DOCUMENT_MAX_TEXT_CHARS themselves — extractDocumentText runs the
// shared truncation pass after the parser returns. Parsers MAY enforce a
// cheaper internal safety bound (e.g. xlsx zip-bomb protection) so they
// don't allocate hundreds of MiB only to throw it away.
type documentParser func(ctx context.Context, raw []byte, opts documentParseOpts) (string, error)

// documentParseOpts carries per-parser configuration that the registry needs
// to thread through from the caller. Add fields here when introducing new
// configurable subprocess paths (e.g. a future DOCUMENT_PANDOC_CMD).
type documentParseOpts struct {
	PdftotextCmd string
}

// documentParsers maps lowercase extension → parser. Anything not listed
// here falls through to the plain-text reader inside extractDocumentText.
var documentParsers = map[string]documentParser{
	"pdf":  parsePDF,
	"docx": pandocParser("docx"),
	"odt":  pandocParser("odt"),
	"pptx": pandocParser("pptx"),
	"epub": pandocParser("epub"),
	"xlsx": parseXLSX,
	"xlsm": parseXLSX,
}

// extractDocumentText returns a UTF-8 string representation of raw according
// to ext. Specialized parsers (PDF, office) live in documentParsers; every
// other whitelisted extension is treated as plain text (invalid UTF-8 is
// replaced with U+FFFD). The returned string is truncated to maxChars runes
// if necessary.
func extractDocumentText(ctx context.Context, ext string, raw []byte, pdftotextCmd string, maxChars int) (string, error) {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	opts := documentParseOpts{PdftotextCmd: pdftotextCmd}

	var (
		text string
		err  error
	)
	if parser, ok := documentParsers[ext]; ok {
		text, err = parser(ctx, raw, opts)
		if err != nil {
			return "", err
		}
	} else {
		if utf8.Valid(raw) {
			text = string(raw)
		} else {
			text = strings.ToValidUTF8(string(raw), "\uFFFD")
		}
	}
	return truncateDocumentText(text, maxChars), nil
}

// parsePDF is a documentParser shim around runPdftotext.
func parsePDF(ctx context.Context, raw []byte, opts documentParseOpts) (string, error) {
	return runPdftotext(ctx, opts.PdftotextCmd, raw)
}

// runPdftotext invokes pdftotext reading from stdin and writing to stdout.
// A 30s timeout is layered on top of the caller's context.
func runPdftotext(ctx context.Context, pdftotextCmd string, raw []byte) (string, error) {
	if strings.TrimSpace(pdftotextCmd) == "" {
		pdftotextCmd = "pdftotext"
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, pdftotextCmd, "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("pdftotext timed out after 30s")
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("pdftotext not installed (command %q not found)", pdftotextCmd)
		}
		trimmed := strings.TrimSpace(stderr.String())
		if trimmed == "" {
			return "", fmt.Errorf("pdftotext failed: %w", err)
		}
		return "", fmt.Errorf("pdftotext failed: %w (%s)", err, trimmed)
	}
	return stdout.String(), nil
}

// parseXLSX extracts an xlsx/xlsm workbook into a per-sheet TSV body.
// TSV is the most compact lossless representation: no markdown alignment
// pipes, no quoting, one row per line, tab-separated cells. Each sheet is
// preceded by a "## Sheet: <name>" header.
//
// Two cheap pre-truncation safety caps protect against xlsx zip-bombs and
// runaway sparse sheets. The shared rune-level truncation in
// extractDocumentText still runs on top, so DOCUMENT_MAX_TEXT_CHARS remains
// the user-visible limit.
func parseXLSX(ctx context.Context, raw []byte, _ documentParseOpts) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("xlsx open failed: %w", err)
	}
	defer f.Close()

	const (
		maxBytes = 4 << 20 // 4 MiB pre-truncation cap
		maxRows  = 50_000
	)
	var buf strings.Builder
	rowsSeen := 0
	truncated := false

	for _, name := range f.GetSheetList() {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if buf.Len() > maxBytes || rowsSeen >= maxRows {
			truncated = true
			break
		}
		rows, err := f.GetRows(name)
		if err != nil {
			return "", fmt.Errorf("xlsx read sheet %q: %w", name, err)
		}
		fmt.Fprintf(&buf, "## Sheet: %s\n", name)
		for _, row := range rows {
			for len(row) > 0 && strings.TrimSpace(row[len(row)-1]) == "" {
				row = row[:len(row)-1]
			}
			if len(row) == 0 {
				continue
			}
			buf.WriteString(strings.Join(row, "\t"))
			buf.WriteByte('\n')
			rowsSeen++
			if buf.Len() > maxBytes || rowsSeen >= maxRows {
				truncated = true
				break
			}
		}
		buf.WriteByte('\n')
	}

	out := buf.String()
	if truncated {
		out += "\n[... xlsx parser stopped early at safety cap]"
	}
	return out, nil
}

// pandocParser returns a documentParser bound to a specific pandoc input
// format (e.g. "docx", "odt", "pptx", "epub").
func pandocParser(format string) documentParser {
	return func(ctx context.Context, raw []byte, _ documentParseOpts) (string, error) {
		return runPandoc(ctx, format, raw)
	}
}

// runPandoc invokes pandoc to convert raw bytes of the given input format
// into GitHub-flavored markdown. Binary (zip-based) inputs are written to a
// temp file because pandoc can be flaky reading them from stdin. The temp
// file is removed on return.
func runPandoc(ctx context.Context, format string, raw []byte) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tmp, err := os.CreateTemp("", "llmtb-doc-*."+format)
	if err != nil {
		return "", fmt.Errorf("pandoc temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return "", fmt.Errorf("pandoc write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("pandoc close temp: %w", err)
	}

	cmd := exec.CommandContext(runCtx, "pandoc",
		"-f", format,
		"-t", "gfm",
		"--wrap=none",
		tmpPath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("pandoc timed out after 30s")
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("pandoc not installed (install with: apk add pandoc)")
		}
		trimmed := strings.TrimSpace(stderr.String())
		if trimmed == "" {
			return "", fmt.Errorf("pandoc failed: %w", err)
		}
		return "", fmt.Errorf("pandoc failed: %w (%s)", err, trimmed)
	}
	return stdout.String(), nil
}

// truncateDocumentText enforces a rune-level cap on s. When truncation fires,
// a trailing "[... truncated, original N chars]" marker is appended.
func truncateDocumentText(s string, maxChars int) string {
	if maxChars <= 0 {
		return s
	}
	total := utf8.RuneCountInString(s)
	if total <= maxChars {
		return s
	}
	i := 0
	count := 0
	for count < maxChars && i < len(s) {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i] + fmt.Sprintf("\n[... truncated, original %d chars]", total)
}
