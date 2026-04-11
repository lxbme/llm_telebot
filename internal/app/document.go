package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// defaultDocumentAllowedExts is the default whitelist of file extensions
// accepted by handleDocument. "pdf" is handled specially (via pdftotext);
// every other entry is read as plain UTF-8 text.
const defaultDocumentAllowedExts = "txt,md,markdown,csv,tsv,json,xml,yaml,yml,toml,ini,log,pdf," +
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

// extractDocumentText returns a UTF-8 string representation of raw according
// to ext. PDFs are handed to pdftotextCmd via stdin/stdout; other extensions
// are treated as plain text (invalid UTF-8 is replaced with U+FFFD).
// The returned string is truncated to maxChars runes if necessary.
func extractDocumentText(ctx context.Context, ext string, raw []byte, pdftotextCmd string, maxChars int) (string, error) {
	ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
	var text string
	switch ext {
	case "pdf":
		out, err := runPdftotext(ctx, pdftotextCmd, raw)
		if err != nil {
			return "", err
		}
		text = out
	default:
		if utf8.Valid(raw) {
			text = string(raw)
		} else {
			text = strings.ToValidUTF8(string(raw), "\uFFFD")
		}
	}
	return truncateDocumentText(text, maxChars), nil
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
	// Walk runes so we cut on a boundary.
	i := 0
	count := 0
	for count < maxChars && i < len(s) {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i] + fmt.Sprintf("\n[... truncated, original %d chars]", total)
}
