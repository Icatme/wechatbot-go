package wechatbot

import (
	"fmt"

	botlog "github.com/Icatme/wechatbot-go/log"
)

func (b *Bot) reportError(err error) {
	b.log("error", "%v", err)
	if b.opts.OnError != nil {
		b.opts.OnError(err)
	}
	if hookErr := b.hooks.OnError.Run(err); hookErr != nil {
		b.log("warn", "OnError hook failed: %v", hookErr)
	}
}

func (b *Bot) log(level, format string, args ...interface{}) {
	if b.opts.LogLevel == "silent" {
		return
	}
	b.mu.Lock()
	logger := b.logger
	b.mu.Unlock()
	if logger == nil {
		logger = newDefaultLogger(b.opts.LogLevel)
	}
	msg := fmt.Sprintf(format, args...)
	lvl := botlog.InfoLevel
	switch level {
	case "debug":
		lvl = botlog.DebugLevel
	case "warn":
		lvl = botlog.WarnLevel
	case "error":
		lvl = botlog.ErrorLevel
	}
	logger.Log(lvl, msg)
}

// SetLogger replaces the default stderr logger with a custom implementation.
func (b *Bot) SetLogger(fn func(level, msg string)) {
	if fn == nil {
		return
	}
	b.mu.Lock()
	b.logger = &legacyLogger{fn: fn}
	b.mu.Unlock()
}

// SetStructuredLogger replaces the default logger with a structured logger.
func (b *Bot) SetStructuredLogger(l *botlog.Logger) {
	if l == nil {
		return
	}
	b.mu.Lock()
	b.logger = l
	b.mu.Unlock()
}

type loggerAdapter interface {
	Log(level botlog.Level, msg string, fields ...botlog.Field)
}

type legacyLogger struct {
	fn func(level, msg string)
}

func (l *legacyLogger) Log(level botlog.Level, msg string, fields ...botlog.Field) {
	l.fn(string(level), msg)
}

func newDefaultLogger(level string) loggerAdapter {
	lvl := botlog.InfoLevel
	switch level {
	case "debug":
		lvl = botlog.DebugLevel
	case "warn":
		lvl = botlog.WarnLevel
	case "error":
		lvl = botlog.ErrorLevel
	}
	return botlog.New(botlog.Options{Level: lvl})
}
