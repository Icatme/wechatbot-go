// Package redact removes credentials and signed URL parameters from text that
// may be exposed through logs or returned errors.
package redact

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

const replacement = "***"

var (
	sensitiveExact = map[string]struct{}{
		"authorization":         {},
		"proxy-authorization":   {},
		"proxy_authorization":   {},
		"token":                 {},
		"credential":            {},
		"password":              {},
		"secret":                {},
		"api_key":               {},
		"apikey":                {},
		"aeskey":                {},
		"aes_key":               {},
		"encrypt_query_param":   {},
		"encrypted_query_param": {},
		"x-encrypted-param":     {},
		"x_encrypted_param":     {},
		"upload_param":          {},
		"thumb_upload_param":    {},
		"upload_full_url":       {},
		"download_full_url":     {},
		"full_url":              {},
		"qrcode":                {},
		"qrcode_url":            {},
		"qrcode_img_content":    {},
		"verify_code":           {},
		"filekey":               {},
		"file_key":              {},
	}
	sensitiveFragments = []string{
		"_token", "token_", "_ticket", "ticket_", "auth_", "_auth",
		"credential", "password", "secret", "api_key", "apikey", "aes_key", "aeskey",
		"upload_param", "full_url",
	}
	authorizationAssignmentPrefixPattern = regexp.MustCompile(`(?i)((?:proxy[-_])?authorization)(\s*=\s*)`)
	authorizationHeaderPattern           = regexp.MustCompile(`(?im)((?:proxy[-_])?authorization)([ \t]*:[ \t]*)[^\r\n]*`)
	assignmentPrefixPattern              = regexp.MustCompile(`(?i)([a-z0-9_.-]+)(\s*=\s*)`)
	headerPrefixPattern                  = regexp.MustCompile(`(?i)([a-z0-9_.-]+)(\s*:\s*)`)
	urlPattern                           = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>]+`)
	schemeRelativeURLPattern             = regexp.MustCompile(`//[^\s"'<>]+`)
)

// SensitiveKey reports whether a structured field name carries secret data.
func SensitiveKey(key string, extra []string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if _, ok := sensitiveExact[lower]; ok {
		return true
	}
	compact := strings.NewReplacer("_", "", "-", "").Replace(lower)
	if strings.HasSuffix(compact, "token") || strings.HasPrefix(compact, "token") ||
		strings.HasSuffix(compact, "ticket") || strings.HasPrefix(compact, "ticket") {
		return true
	}
	for _, fragment := range sensitiveFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	for _, candidate := range extra {
		if strings.EqualFold(strings.TrimSpace(candidate), key) {
			return true
		}
	}
	return false
}

// String redacts sensitive JSON fields, assignments, headers, and URL query
// strings. Valid JSON is handled recursively; malformed JSON is handled on a
// best-effort basis.
func String(input string, extra []string) string {
	if input == "" {
		return input
	}
	if value, ok := decodeJSON(input); ok {
		redacted, err := json.Marshal(redactValue(value, extra))
		if err == nil {
			return string(redacted)
		}
	}
	return redactText(input, extra)
}

// URL keeps a URL's location useful for diagnostics while removing userinfo,
// query values, and fragments, all of which can contain credentials.
func URL(raw string) string {
	parsed, err := url.Parse(raw)
	if err == nil {
		if parsed.User != nil {
			parsed.User = url.User(replacement)
		}
		if parsed.RawQuery != "" || parsed.ForceQuery {
			parsed.RawQuery = replacement
			parsed.ForceQuery = true
		}
		if parsed.Fragment != "" {
			parsed.Fragment = replacement
		}
		return parsed.String()
	}
	safe := redactMalformedURLUserinfo(raw)
	if query := strings.IndexByte(safe, '?'); query >= 0 {
		if fragment := strings.IndexByte(safe[query:], '#'); fragment >= 0 {
			return safe[:query] + "?" + replacement + "#" + replacement
		}
		return safe[:query] + "?" + replacement
	}
	if fragment := strings.IndexByte(safe, '#'); fragment >= 0 {
		safe = safe[:fragment] + "#" + replacement
	}
	return redactTextWithoutURLs(safe, nil)
}

func redactMalformedURLUserinfo(raw string) string {
	scheme := strings.Index(raw, "://")
	authorityStart := 0
	if scheme >= 0 {
		authorityStart = scheme + 3
	} else if strings.HasPrefix(raw, "//") {
		authorityStart = 2
	} else {
		return raw
	}
	authorityEnd := len(raw)
	if end := strings.IndexAny(raw[authorityStart:], "/?#"); end >= 0 {
		authorityEnd = authorityStart + end
	}
	userinfo := strings.LastIndexByte(raw[authorityStart:authorityEnd], '@')
	if userinfo < 0 {
		return raw
	}
	return raw[:authorityStart] + replacement + "@" + raw[authorityStart+userinfo+1:]
}

// Error returns an error with a sanitized Error string. Safe causes remain
// available to errors.Is/errors.As traversal, while unsafe wrappers are
// rebuilt without retaining the original secret-bearing object. URL errors
// are cloned with a sanitized URL so errors.As exposes the safe form.
func Error(err error) error {
	if err == nil {
		return nil
	}
	if urlErr, ok := err.(*url.Error); ok {
		return sanitizeURLError(urlErr)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		message := String(err.Error(), nil)
		if causes, ok := err.(interface{ Unwrap() []error }); ok {
			safeCauses := make([]error, 0, len(causes.Unwrap()))
			for _, cause := range causes.Unwrap() {
				safeCauses = append(safeCauses, Error(cause))
			}
			return sanitizedMultiError{message: message, causes: safeCauses}
		}
		if cause := errors.Unwrap(err); cause != nil {
			return sanitizedError{message: message, cause: Error(cause)}
		}
		return sanitizedError{message: message, cause: sanitizeURLError(urlErr)}
	}
	message := String(err.Error(), nil)
	if causes, ok := err.(interface{ Unwrap() []error }); ok {
		originalCauses := causes.Unwrap()
		safeCauses := make([]error, 0, len(originalCauses))
		changed := message != err.Error()
		for _, cause := range originalCauses {
			safeCause := Error(cause)
			safeCauses = append(safeCauses, safeCause)
			changed = changed || !sameError(safeCause, cause)
		}
		if !changed {
			return err
		}
		return sanitizedMultiError{message: message, causes: safeCauses}
	}
	if cause := errors.Unwrap(err); cause != nil {
		safeCause := Error(cause)
		if message == err.Error() && sameError(safeCause, cause) {
			return err
		}
		return sanitizedError{message: message, cause: safeCause}
	}
	if message == err.Error() {
		return err
	}
	return sanitizedError{message: message}
}

func sameError(left, right error) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	return leftValue.Type() == rightValue.Type() && leftValue.Comparable() && leftValue.Equal(rightValue)
}

func sanitizeURLError(err *url.Error) *url.Error {
	return &url.Error{
		Op:  String(err.Op, nil),
		URL: URL(err.URL),
		Err: Error(err.Err),
	}
}

type sanitizedError struct {
	message string
	cause   error
}

func (e sanitizedError) Error() string { return e.message }
func (e sanitizedError) Unwrap() error { return e.cause }

type sanitizedMultiError struct {
	message string
	causes  []error
}

func (e sanitizedMultiError) Error() string   { return e.message }
func (e sanitizedMultiError) Unwrap() []error { return e.causes }

func decodeJSON(input string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return value, true
}

func redactValue(value any, extra []string) any {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if SensitiveKey(key, extra) {
				current[key] = replacement
				continue
			}
			current[key] = redactValue(child, extra)
		}
		return current
	case []any:
		for index, child := range current {
			current[index] = redactValue(child, extra)
		}
		return current
	case string:
		return redactText(current, extra)
	default:
		return value
	}
}

func redactText(input string, extra []string) string {
	redacted := replaceSensitiveJSONFields(input, extra)
	redacted = urlPattern.ReplaceAllStringFunc(redacted, URL)
	redacted = schemeRelativeURLPattern.ReplaceAllStringFunc(redacted, URL)
	redacted = replaceAuthorizationValues(redacted)
	redacted = replaceSensitivePairs(redacted, assignmentPrefixPattern, extra)
	return replaceSensitivePairs(redacted, headerPrefixPattern, extra)
}

func redactTextWithoutURLs(input string, extra []string) string {
	redacted := replaceSensitiveJSONFields(input, extra)
	redacted = replaceAuthorizationValues(redacted)
	redacted = replaceSensitivePairs(redacted, assignmentPrefixPattern, extra)
	return replaceSensitivePairs(redacted, headerPrefixPattern, extra)
}

func replaceAuthorizationValues(input string) string {
	redacted := replaceAuthorizationAssignments(input)
	return replaceAuthorizationValue(redacted, authorizationHeaderPattern)
}

func replaceAuthorizationAssignments(input string) string {
	matches := authorizationAssignmentPrefixPattern.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input
	}
	var redacted strings.Builder
	redacted.Grow(len(input))
	cursor := 0
	for _, match := range matches {
		if len(match) < 2 || match[0] < cursor {
			continue
		}
		valueStart := match[1]
		if valueStart >= len(input) {
			continue
		}
		if input[valueStart] == '"' || input[valueStart] == '\'' {
			redacted.WriteString(input[cursor:valueStart])
			redacted.WriteString(replacement)
			cursor = quotedValueEnd(input, valueStart)
			continue
		}
		firstEnd := authorizationTokenEnd(input, valueStart)
		if firstEnd == valueStart {
			continue
		}
		valueEnd := firstEnd
		secondStart := skipInlineSpace(input, firstEnd)
		secondEnd := authorizationTokenEnd(input, secondStart)
		if isAuthorizationScheme(input[valueStart:firstEnd]) && secondEnd > secondStart {
			if !isCommonDiagnosticAssignment(input[secondStart:secondEnd]) {
				valueEnd = authorizationValueEnd(input, secondStart)
			}
		}
		redacted.WriteString(input[cursor:valueStart])
		redacted.WriteString(replacement)
		cursor = valueEnd
	}
	redacted.WriteString(input[cursor:])
	return redacted.String()
}

func quotedValueEnd(input string, start int) int {
	quote := input[start]
	escaped := false
	for offset := start + 1; offset < len(input); offset++ {
		if escaped {
			escaped = false
			continue
		}
		if input[offset] == '\\' {
			escaped = true
		} else if input[offset] == quote {
			return offset + 1
		}
	}
	return len(input)
}

func authorizationTokenEnd(input string, start int) int {
	end := start
	for end < len(input) && !strings.ContainsRune(" \t\r\n&,;", rune(input[end])) {
		end++
	}
	return end
}

func skipInlineSpace(input string, start int) int {
	for start < len(input) && (input[start] == ' ' || input[start] == '\t') {
		start++
	}
	return start
}

func isAuthorizationScheme(value string) bool {
	if value == "" || !isASCIILetter(value[0]) {
		return false
	}
	for offset := 1; offset < len(value); offset++ {
		current := value[offset]
		if !isASCIILetter(current) && (current < '0' || current > '9') && !strings.ContainsRune("+._-", rune(current)) {
			return false
		}
	}
	return true
}

func isASCIILetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func looksLikeDiagnosticAssignment(value string) bool {
	equals := strings.IndexByte(value, '=')
	if equals <= 0 {
		return false
	}
	return strings.Trim(value[equals:], "=") != ""
}

func authorizationValueEnd(input string, start int) int {
	for offset := start; offset < len(input); {
		if strings.ContainsRune("&;\r\n", rune(input[offset])) {
			return offset
		}
		if input[offset] != ' ' && input[offset] != '\t' && input[offset] != ',' {
			offset++
			continue
		}
		separator := offset
		for offset < len(input) && (input[offset] == ' ' || input[offset] == '\t' || input[offset] == ',') {
			offset++
		}
		candidateEnd := authorizationTokenEnd(input, offset)
		if candidateEnd > offset && isCommonDiagnosticAssignment(input[offset:candidateEnd]) {
			return separator
		}
		offset = candidateEnd
	}
	return len(input)
}

func isCommonDiagnosticAssignment(value string) bool {
	if !looksLikeDiagnosticAssignment(value) {
		return false
	}
	key := strings.ToLower(value[:strings.IndexByte(value, '=')])
	switch key {
	case "status", "http", "http_status", "ret", "errcode", "code", "error", "error_code", "endpoint", "method", "attempt":
		return true
	default:
		return false
	}
}

func replaceAuthorizationValue(input string, pattern *regexp.Regexp) string {
	return pattern.ReplaceAllStringFunc(input, func(value string) string {
		parts := pattern.FindStringSubmatchIndex(value)
		if len(parts) < 6 {
			return value
		}
		return value[:parts[5]] + replacement
	})
}

func replaceSensitiveJSONFields(input string, extra []string) string {
	var redacted strings.Builder
	redacted.Grow(len(input))
	for offset := 0; offset < len(input); {
		if input[offset] != '"' {
			redacted.WriteByte(input[offset])
			offset++
			continue
		}

		keyEnd, complete := jsonStringEnd(input, offset)
		if !complete {
			redacted.WriteString(input[offset:])
			break
		}
		key, err := strconv.Unquote(input[offset:keyEnd])
		colon := skipJSONSpace(input, keyEnd)
		if err != nil || colon >= len(input) || input[colon] != ':' || !SensitiveKey(key, extra) {
			redacted.WriteString(input[offset:keyEnd])
			offset = keyEnd
			continue
		}

		valueStart := skipJSONSpace(input, colon+1)
		redacted.WriteString(input[offset:valueStart])
		redacted.WriteString(`"` + replacement + `"`)
		offset = jsonValueEnd(input, valueStart)
	}
	return redacted.String()
}

func jsonStringEnd(input string, start int) (int, bool) {
	escaped := false
	for offset := start + 1; offset < len(input); offset++ {
		if escaped {
			escaped = false
			continue
		}
		switch input[offset] {
		case '\\':
			escaped = true
		case '"':
			return offset + 1, true
		}
	}
	return len(input), false
}

func skipJSONSpace(input string, offset int) int {
	for offset < len(input) {
		switch input[offset] {
		case ' ', '\t', '\n', '\r':
			offset++
		default:
			return offset
		}
	}
	return offset
}

func jsonValueEnd(input string, start int) int {
	if start >= len(input) {
		return start
	}
	if input[start] == '"' {
		if end, complete := jsonStringEnd(input, start); complete {
			return end
		}
		return len(input)
	}
	if input[start] != '{' && input[start] != '[' {
		offset := start
		for offset < len(input) && !strings.ContainsRune(",}] \t\n\r", rune(input[offset])) {
			offset++
		}
		return offset
	}

	objectDepth, arrayDepth := 0, 0
	inString, escaped := false, false
	for offset := start; offset < len(input); offset++ {
		current := input[offset]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}
		switch current {
		case '"':
			inString = true
		case '{':
			objectDepth++
		case '[':
			arrayDepth++
		case '}':
			if objectDepth > 0 {
				objectDepth--
			}
		case ']':
			if arrayDepth > 0 {
				arrayDepth--
			}
		}
		if objectDepth == 0 && arrayDepth == 0 {
			return offset + 1
		}
	}
	return len(input)
}

func replaceSensitivePairs(input string, pattern *regexp.Regexp, extra []string) string {
	matches := pattern.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input
	}
	var redacted strings.Builder
	redacted.Grow(len(input))
	cursor := 0
	for _, match := range matches {
		if len(match) < 6 || match[0] < cursor || !SensitiveKey(input[match[2]:match[3]], extra) {
			continue
		}
		valueStart := match[1]
		if valueStart >= len(input) {
			continue
		}
		valueEnd := sensitivePairValueEnd(input, valueStart)
		if valueEnd == valueStart {
			continue
		}
		redacted.WriteString(input[cursor:valueStart])
		redacted.WriteString(replacement)
		cursor = valueEnd
	}
	if cursor == 0 {
		return input
	}
	redacted.WriteString(input[cursor:])
	return redacted.String()
}

func sensitivePairValueEnd(input string, start int) int {
	if input[start] == '"' || input[start] == '\'' {
		return quotedValueEnd(input, start)
	}
	for offset := start; offset < len(input); {
		if strings.ContainsRune("&\r\n", rune(input[offset])) {
			return offset
		}
		if input[offset] != ' ' && input[offset] != '\t' && input[offset] != ',' && input[offset] != ';' {
			offset++
			continue
		}
		separator := offset
		hasWhitespace := false
		for offset < len(input) && (input[offset] == ' ' || input[offset] == '\t' || input[offset] == ',' || input[offset] == ';') {
			hasWhitespace = hasWhitespace || input[offset] == ' ' || input[offset] == '\t'
			offset++
		}
		candidateEnd := authorizationTokenEnd(input, offset)
		if candidateEnd > offset && looksLikeDiagnosticAssignment(input[offset:candidateEnd]) && (hasWhitespace || isCommonDiagnosticAssignment(input[offset:candidateEnd])) {
			return separator
		}
		offset = candidateEnd
	}
	return len(input)
}
