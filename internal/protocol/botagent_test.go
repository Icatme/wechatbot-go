package protocol

import (
	"strings"
	"testing"
)

func TestSanitizeBotAgent(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "OpenClaw"},
		{"   ", "OpenClaw"},
		{"MyBot/1.2.0", "MyBot/1.2.0"},
		{"MyBot/1.2.0 (region=cn;env=prod)", "MyBot/1.2.0 (region=cn;env=prod)"},
		{"MyBot/1.2.0 LangChain/0.3.5", "MyBot/1.2.0 LangChain/0.3.5"},
		{"MyBot/1.2.0-rc.1+build.5", "MyBot/1.2.0-rc.1+build.5"},
		{"invalid token here", "OpenClaw"},
		{"MyBot/1.0 invalid Other/2.0", "MyBot/1.0 Other/2.0"},
		{"BadName!/1.0", "OpenClaw"},
	}

	for _, tc := range tests {
		got := SanitizeBotAgent(tc.input)
		if got != tc.want {
			t.Errorf("SanitizeBotAgent(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSanitizeBotAgentTruncation(t *testing.T) {
	// Construct a 300-byte input that should be truncated to under 256 bytes.
	input := strings.Repeat("A/1.0 ", 50)
	got := SanitizeBotAgent(input)
	if len(got) > maxBotAgentLen {
		t.Fatalf("sanitized length %d exceeds max %d", len(got), maxBotAgentLen)
	}
	if got == "" || got == "OpenClaw" {
		t.Fatal("expected some tokens to remain after truncation")
	}
}
