// Package log provides structured logging with automatic token redaction.
package log

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level indicates the severity of a log entry.
type Level string

const (
	DebugLevel Level = "debug"
	InfoLevel  Level = "info"
	WarnLevel  Level = "warn"
	ErrorLevel Level = "error"
)

// Field is a structured log field.
type Field struct {
	Key   string
	Value interface{}
}

// F returns a Field.
func F(key string, value interface{}) Field {
	return Field{Key: key, Value: value}
}

// Logger writes structured, redacted log entries.
type Logger struct {
	level    Level
	out      io.Writer
	redactor *strings.Replacer
	mu       sync.Mutex
}

// Options configures a Logger.
type Options struct {
	Level  Level
	Output io.Writer
	// ExtraSensitiveKeys lists additional keys whose values should always be redacted.
	ExtraSensitiveKeys []string
}

// New creates a new Logger. If level is empty, defaults to InfoLevel.
func New(opts Options) *Logger {
	if opts.Level == "" {
		opts.Level = InfoLevel
	}
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	return &Logger{
		level:    opts.Level,
		out:      opts.Output,
		redactor: buildRedactor(opts.ExtraSensitiveKeys),
	}
}

// IsEnabled reports whether the given level is enabled.
func (l *Logger) IsEnabled(level Level) bool {
	return levelPriority(level) >= levelPriority(l.level)
}

// Debug logs a debug message.
func (l *Logger) Debug(msg string, fields ...Field) { l.Log(DebugLevel, msg, fields...) }

// Info logs an info message.
func (l *Logger) Info(msg string, fields ...Field) { l.Log(InfoLevel, msg, fields...) }

// Warn logs a warning message.
func (l *Logger) Warn(msg string, fields ...Field) { l.Log(WarnLevel, msg, fields...) }

// Error logs an error message.
func (l *Logger) Error(msg string, fields ...Field) { l.Log(ErrorLevel, msg, fields...) }

// Log writes a log entry at the given level.
func (l *Logger) Log(level Level, msg string, fields ...Field) {
	if !l.IsEnabled(level) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var b strings.Builder
	b.WriteString("{")
	writeJSONField(&b, "time", now, false)
	b.WriteString(",")
	writeJSONField(&b, "level", string(level), false)
	b.WriteString(",")
	writeJSONField(&b, "msg", redactMessage(l.redact(msg)), false)
	for _, f := range fields {
		b.WriteString(",")
		key := sanitizeKey(f.Key)
		if isSensitiveKey(f.Key) {
			writeJSONField(&b, key, "***", false)
			continue
		}
		writeValue(&b, key, f.Value, l)
	}
	b.WriteString("}\n")

	l.mu.Lock()
	_, _ = l.out.Write([]byte(b.String()))
	l.mu.Unlock()
}

func writeJSONField(b *strings.Builder, key, value string, isNumber bool) {
	b.WriteString(strconv.Quote(key))
	b.WriteString(":")
	if isNumber {
		b.WriteString(value)
	} else {
		b.WriteString(strconv.Quote(value))
	}
}

func writeValue(b *strings.Builder, key string, v interface{}, l *Logger) {
	b.WriteString(strconv.Quote(key))
	b.WriteString(":")
	switch x := v.(type) {
	case string:
		b.WriteString(strconv.Quote(l.redact(x)))
	case error:
		b.WriteString(strconv.Quote(l.redact(x.Error())))
	case int:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case uint:
		b.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		b.WriteString(strconv.FormatUint(x, 10))
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
	case float32:
		b.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	default:
		data, err := json.Marshal(x)
		if err != nil {
			b.WriteString(strconv.Quote(l.redact(fmt.Sprintf("%v", x))))
			return
		}
		b.WriteString(l.redact(string(data)))
	}
}

func (l *Logger) redact(s string) string {
	return l.redactor.Replace(s)
}

func sanitizeKey(key string) string {
	key = strings.ReplaceAll(key, "\x00", "")
	return key
}

var sensitiveKeyPatterns = []string{
	"_token", "token_", "bot_token", "context_token", "typing_ticket",
	"auth_", "_auth", "credential", "password", "secret", "api_key", "apikey",
	"aes_key", "encrypt_query_param", "filekey",
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, p := range sensitiveKeyPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func buildRedactor(extra []string) *strings.Replacer {
	// Redact common token patterns in free-form strings.
	replacements := []string{
		"bot_token=", "bot_token=***",
		"context_token=", "context_token=***",
		"typing_ticket=", "typing_ticket=***",
		"encrypt_query_param=", "encrypt_query_param=***",
		"filekey=", "filekey=***",
	}
	for _, k := range extra {
		replacements = append(replacements, k+"=", k+"=***")
	}
	return strings.NewReplacer(replacements...)
}

var tokenPattern = regexp.MustCompile(`\b(bot_token|context_token|typing_ticket|encrypt_query_param|filekey)=[^\s&"]+`)

// RedactString returns a copy of s with known token query parameters redacted.
func RedactString(s string) string {
	return tokenPattern.ReplaceAllString(s, "$1=***")
}

// redactMessage scans a free-form message and redacts embedded token values.
func redactMessage(s string) string {
	return tokenPattern.ReplaceAllString(s, "$1=***")
}

func levelPriority(level Level) int {
	switch level {
	case DebugLevel:
		return 0
	case InfoLevel:
		return 1
	case WarnLevel:
		return 2
	case ErrorLevel:
		return 3
	}
	return 1
}
