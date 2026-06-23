package protocol

import (
	"regexp"
	"strings"
)

const (
	defaultBotAgent = "OpenClaw"
	maxBotAgentLen  = 256
)

var (
	productRe     = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}/[A-Za-z0-9_.+\-]{1,32}$`)
	commentCharRe = regexp.MustCompile(`^[\x20-\x27\x2A-\x7E]{1,64}$`)
)

// SanitizeBotAgent cleans a user-supplied bot_agent value into a wire-safe UA-style string.
// Invalid tokens are dropped; if nothing remains the default "OpenClaw" is returned.
// Grammar: bot_agent = product *( SP product ) where each product may be followed by " (comment)".
func SanitizeBotAgent(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultBotAgent
	}

	// Tokenize on whitespace, but keep multi-word comments glued to the preceding product.
	rawTokens := strings.Fields(trimmed)
	tokens := make([]string, 0, len(rawTokens))
	for i := 0; i < len(rawTokens); i++ {
		tok := rawTokens[i]
		if strings.HasPrefix(tok, "(") && !strings.HasSuffix(tok, ")") {
			acc := tok
			for i+1 < len(rawTokens) && !strings.HasSuffix(acc, ")") {
				i++
				acc += " " + rawTokens[i]
			}
			tokens = append(tokens, acc)
		} else {
			tokens = append(tokens, tok)
		}
	}

	accepted := make([]string, 0, len(tokens))
	var pendingProduct string

	for _, tok := range tokens {
		if strings.HasPrefix(tok, "(") && strings.HasSuffix(tok, ")") {
			inner := tok[1 : len(tok)-1]
			if pendingProduct != "" && commentCharRe.MatchString(inner) {
				accepted = append(accepted, pendingProduct+" ("+inner+")")
				pendingProduct = ""
				continue
			}
			if pendingProduct != "" {
				accepted = append(accepted, pendingProduct)
				pendingProduct = ""
			}
			continue
		}

		if pendingProduct != "" {
			accepted = append(accepted, pendingProduct)
			pendingProduct = ""
		}

		if productRe.MatchString(tok) {
			pendingProduct = tok
		}
	}
	if pendingProduct != "" {
		accepted = append(accepted, pendingProduct)
	}

	if len(accepted) == 0 {
		return defaultBotAgent
	}

	joined := strings.Join(accepted, " ")
	if len(joined) <= maxBotAgentLen {
		return joined
	}

	// Truncate by dropping trailing tokens.
	truncated := make([]string, 0, len(accepted))
	length := 0
	for _, t := range accepted {
		add := len(t)
		if len(truncated) > 0 {
			add++ // space separator
		}
		if length+add > maxBotAgentLen {
			break
		}
		truncated = append(truncated, t)
		length += add
	}
	if len(truncated) == 0 {
		return defaultBotAgent
	}
	return strings.Join(truncated, " ")
}
