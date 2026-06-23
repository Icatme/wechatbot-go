// Package markdown provides text normalization for WeChat messages.
package markdown

import (
	"regexp"
	"strings"
)

var (
	// Code blocks: strip fences, keep content.
	codeBlockRe = regexp.MustCompile("```[^\\n]*\\n?([\\s\\S]*?)```")
	// Inline code: strip backticks, keep content.
	inlineCodeRe = regexp.MustCompile("`([^`]+)`")
	// Images: remove entirely.
	imageRe = regexp.MustCompile("!\\[[^\\]]*\\]\\([^)]*\\)")
	// Links: keep display text only.
	linkRe = regexp.MustCompile("\\[([^\\]]+)\\]\\([^)]*\\)")
	// Tables: remove separator rows.
	tableSepRe = regexp.MustCompile("(?m)^\\|[-:|\\s]+\\|$")
	// Table rows: clean pipes.
	tableRowRe = regexp.MustCompile("(?m)^\\|(.+)\\|$")
	// Headers: strip # prefix.
	headerRe = regexp.MustCompile("(?m)^#{1,6}\\s+")
	// Horizontal rules.
	hrRe = regexp.MustCompile("(?m)^[-*_]{3,}\\s*$")
	// Bold/italic.
	bold3Re   = regexp.MustCompile("\\*\\*\\*(.+?)\\*\\*\\*")
	boldRe    = regexp.MustCompile("\\*\\*(.+?)\\*\\*")
	italicRe  = regexp.MustCompile("\\*(.+?)\\*")
	ubold3Re  = regexp.MustCompile("___(.+?)___")
	uboldRe   = regexp.MustCompile("__(.+?)__")
	uitalicRe = regexp.MustCompile("_(.+?)_")
	strikeRe  = regexp.MustCompile("~~(.+?)~~")
	// Blockquotes.
	quoteRe = regexp.MustCompile("(?m)^>\\s?")
	// Unordered lists: replace bullet markers with •.
	ulRe = regexp.MustCompile("(?m)^[\\s]*[-*+]\\s+")
	// Ordered lists: strip number markers.
	olRe = regexp.MustCompile("(?m)^[\\s]*\\d+\\.\\s+")
	// HTML tags.
	htmlRe = regexp.MustCompile("<[^>]+>")
)

// StripMarkdown removes markdown syntax while preserving content.
// It is intentionally conservative: constructs like code blocks and tables
// are normalized rather than mangled.
func StripMarkdown(text string) string {
	if text == "" {
		return ""
	}

	// Code blocks: keep inner content.
	text = codeBlockRe.ReplaceAllString(text, "$1")
	// Inline code: keep content.
	text = inlineCodeRe.ReplaceAllString(text, "$1")
	// Images: remove entirely.
	text = imageRe.ReplaceAllString(text, "")
	// Links: keep display text.
	text = linkRe.ReplaceAllString(text, "$1")
	// Tables.
	text = tableSepRe.ReplaceAllString(text, "")
	text = tableRowRe.ReplaceAllStringFunc(text, func(line string) string {
		inner := line[1 : len(line)-1]
		cells := strings.Split(inner, "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		return strings.Join(cells, "  ")
	})
	// Headers.
	text = headerRe.ReplaceAllString(text, "")
	// Horizontal rules.
	text = hrRe.ReplaceAllString(text, "")
	// Bold/italic/strikethrough: keep content.
	text = bold3Re.ReplaceAllString(text, "$1")
	text = boldRe.ReplaceAllString(text, "$1")
	text = italicRe.ReplaceAllString(text, "$1")
	text = ubold3Re.ReplaceAllString(text, "$1")
	text = uboldRe.ReplaceAllString(text, "$1")
	text = uitalicRe.ReplaceAllString(text, "$1")
	text = strikeRe.ReplaceAllString(text, "$1")
	// Blockquotes.
	text = quoteRe.ReplaceAllString(text, "")
	// Lists.
	text = ulRe.ReplaceAllString(text, "• ")
	text = olRe.ReplaceAllString(text, "")
	// HTML tags.
	text = htmlRe.ReplaceAllString(text, "")

	// Clean up multiple blank lines created by removed constructs.
	text = collapseBlankLines(text)
	return strings.TrimSpace(text)
}

func collapseBlankLines(text string) string {
	var result []string
	prevBlank := false
	for _, line := range strings.Split(text, "\n") {
		blank := strings.TrimSpace(line) == ""
		if blank && prevBlank {
			continue
		}
		result = append(result, line)
		prevBlank = blank
	}
	return strings.Join(result, "\n")
}
