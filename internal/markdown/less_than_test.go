package markdown

import "testing"

func TestNormalizeBareLessThan(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "comparison", input: "当CPU使用率<80%时属于**正常范围**", want: "当CPU使用率＜80%时属于**正常范围**"},
		{name: "trailing", input: "value<", want: "value＜"},
		{name: "multiple", input: "0<x<10", want: "0＜x＜10"},
		{name: "start tag", input: "see <div>ok</div>", want: "see <div>ok</div>"},
		{name: "tag with attributes", input: `<a title="1 > 0" href="/">ok</a>`, want: `<a title="1 > 0" href="/">ok</a>`},
		{name: "self closing tag", input: "line<br/>next<br />done", want: "line<br/>next<br />done"},
		{name: "case insensitive allowed tag", input: "<SPAN>ok</SPAN>", want: "<SPAN>ok</SPAN>"},
		{name: "comparison-like pseudo tag", input: "a<b and c>d", want: "a＜b and c>d"},
		{name: "unsupported tag", input: "see <section>ok</section>", want: "see ＜section>ok＜/section>"},
		{name: "tag-like comparison", input: "value<80>", want: "value＜80>"},
		{name: "unterminated tag", input: "value<than", want: "value＜than"},
		{name: "fenced code", input: "```go\nif (x < 80) {}\n```\nrate<80", want: "```go\nif (x < 80) {}\n```\nrate＜80"},
		{name: "tilde fence", input: "~~~go\nif (x < 80) {}\n~~~\nrate<80", want: "~~~go\nif (x < 80) {}\n~~~\nrate＜80"},
		{name: "long fence ignores inner triples", input: "````go\n```\nif (x < 80) {}\n````\nrate<80", want: "````go\n```\nif (x < 80) {}\n````\nrate＜80"},
		{name: "longer closing fence", input: "```go\nif (x < 80) {}\n`````\nrate<80", want: "```go\nif (x < 80) {}\n`````\nrate＜80"},
		{name: "inline triples are prose", input: "text ``` x < 80 ``` end", want: "text ``` x ＜ 80 ``` end"},
		{name: "four-space marker is prose", input: "    ``` x < 80", want: "    ``` x ＜ 80"},
		{name: "existing full width", input: "value＜80", want: "value＜80"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeBareLessThan(tc.input); got != tc.want {
				t.Fatalf("NormalizeBareLessThan(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
