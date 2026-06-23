package wechatbot

import (
	"context"
	"strings"
)

// CommandFunc is the signature for a slash command handler.
// ctx is the request context. msg is the incoming message.
// args is the command arguments (excluding the command itself).
// The returned bool indicates whether the command consumed the message.
type CommandFunc func(ctx context.Context, msg *IncomingMessage, args string) bool

// CommandRegistry registers and routes slash commands.
type CommandRegistry struct {
	prefix   string
	commands map[string]CommandFunc
}

// NewCommandRegistry creates a registry with the given command prefix.
// Use "/" for Discord-style slash commands or another prefix as needed.
func NewCommandRegistry(prefix string) *CommandRegistry {
	if prefix == "" {
		prefix = "/"
	}
	return &CommandRegistry{
		prefix:   prefix,
		commands: make(map[string]CommandFunc),
	}
}

// Register adds or replaces a command handler.
func (r *CommandRegistry) Register(name string, fn CommandFunc) {
	if r == nil {
		return
	}
	name = strings.ToLower(strings.TrimSpace(name))
	r.commands[name] = fn
}

// Handle inspects a message and dispatches to a registered command.
// It returns true if a command was found and handled.
func (r *CommandRegistry) Handle(ctx context.Context, msg *IncomingMessage) bool {
	if r == nil || msg == nil || msg.Text == "" {
		return false
	}
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, r.prefix) {
		return false
	}
	body := strings.TrimPrefix(text, r.prefix)
	name, args, _ := strings.Cut(body, " ")
	name = strings.ToLower(strings.TrimSpace(name))
	args = strings.TrimSpace(args)
	fn, ok := r.commands[name]
	if !ok {
		return false
	}
	return fn(ctx, msg, args)
}

// Names returns all registered command names.
func (r *CommandRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.commands))
	for name := range r.commands {
		names = append(names, name)
	}
	return names
}
