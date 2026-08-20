package markdown

import "strings"

const fullWidthLessThan = '\uFF1C'

var allowedHTMLTags = map[string]struct{}{
	"a":    {},
	"br":   {},
	"div":  {},
	"span": {},
}

// NormalizeBareLessThan replaces less-than signs that WeChat would interpret
// as invalid HTML tag starts. A small set of HTML tags and fenced code are
// retained; the SDK does not otherwise promise rich-HTML rendering.
func NormalizeBareLessThan(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}

	var normalized strings.Builder
	normalized.Grow(len(text))
	var fenceMarker byte
	var fenceLength int
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += start + 1
		}
		line := text[start:end]
		fenceLine := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")

		if fenceMarker != 0 {
			normalized.WriteString(line)
			if isClosingFence(fenceLine, fenceMarker, fenceLength) {
				fenceMarker = 0
				fenceLength = 0
			}
		} else if marker, length, ok := openingFence(fenceLine); ok {
			normalized.WriteString(line)
			fenceMarker = marker
			fenceLength = length
		} else {
			normalizeLine(&normalized, line)
		}
		start = end
	}
	return normalized.String()
}

func normalizeLine(normalized *strings.Builder, line string) {
	for i := 0; i < len(line); {
		if line[i] == '<' {
			if end := htmlTagEnd(line, i); end >= 0 {
				normalized.WriteString(line[i:end])
				i = end
				continue
			}
			normalized.WriteRune(fullWidthLessThan)
			i++
			continue
		}
		normalized.WriteByte(line[i])
		i++
	}
}

func openingFence(line string) (byte, int, bool) {
	marker, length, rest, ok := fenceRun(line)
	if !ok || marker == '`' && strings.ContainsRune(rest, '`') {
		return 0, 0, false
	}
	return marker, length, true
}

func isClosingFence(line string, marker byte, openingLength int) bool {
	closingMarker, length, rest, ok := fenceRun(line)
	return ok && closingMarker == marker && length >= openingLength && strings.Trim(rest, " \t") == ""
}

func fenceRun(line string) (byte, int, string, bool) {
	i := 0
	for i < len(line) && i < 4 && line[i] == ' ' {
		i++
	}
	if i > 3 || i >= len(line) || line[i] != '`' && line[i] != '~' {
		return 0, 0, "", false
	}
	marker := line[i]
	start := i
	for i < len(line) && line[i] == marker {
		i++
	}
	if i-start < 3 {
		return 0, 0, "", false
	}
	return marker, i - start, line[i:], true
}

func htmlTagEnd(text string, start int) int {
	i := start + 1
	closing := false
	if i < len(text) && text[i] == '/' {
		closing = true
		i++
	}
	if i >= len(text) || !isASCIILetter(text[i]) {
		return -1
	}
	nameStart := i
	for i++; i < len(text) && isHTMLNameByte(text[i]); i++ {
	}
	if _, ok := allowedHTMLTags[strings.ToLower(text[nameStart:i])]; !ok {
		return -1
	}
	if i >= len(text) {
		return -1
	}
	if text[i] == '>' {
		return i + 1
	}
	if !closing && text[i] == '/' && i+1 < len(text) && text[i+1] == '>' {
		return i + 2
	}
	if !isHTMLSpace(text[i]) {
		return -1
	}

	if closing {
		for i < len(text) && isHTMLSpace(text[i]) {
			i++
		}
		if i < len(text) && text[i] == '>' {
			return i + 1
		}
		return -1
	}

	var quote byte
	for ; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '>':
			return i + 1
		case '<', '\n', '\r':
			return -1
		}
	}
	return -1
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isHTMLNameByte(c byte) bool {
	return isASCIILetter(c) || c >= '0' && c <= '9' || c == '-' || c == ':'
}

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
