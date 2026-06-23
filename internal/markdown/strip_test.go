package markdown

import "testing"

func TestStripMarkdown(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"plain text", "plain text"},
		{"**bold**", "bold"},
		{"*italic*", "italic"},
		{"***bold italic***", "bold italic"},
		{"`code`", "code"},
		{"[link](https://example.com)", "link"},
		{"![alt](https://example.com/a.png)", ""},
		{"# heading", "heading"},
		{"## heading", "heading"},
		{"> quote", "quote"},
		{"- item", "• item"},
		{"* item", "• item"},
		{"1. item", "item"},
		{"~~strike~~", "strike"},
		{"---", ""},
		{"**bold** and *italic*", "bold and italic"},
		{"text with `inline code` here", "text with inline code here"},
	}

	for _, tc := range tests {
		got := StripMarkdown(tc.input)
		if got != tc.want {
			t.Errorf("StripMarkdown(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestStripMarkdownCodeBlock(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")\n```"
	got := StripMarkdown(input)
	want := "fmt.Println(\"hi\")"
	if got != want {
		t.Errorf("StripMarkdown code block = %q, want %q", got, want)
	}
}

func TestStripMarkdownTable(t *testing.T) {
	input := "| a | b |\n|---|---|\n| 1 | 2 |"
	got := StripMarkdown(input)
	want := "a  b\n\n1  2"
	if got != want {
		t.Errorf("StripMarkdown table = %q, want %q", got, want)
	}
}
