package wechatbot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Icatme/wechatbot-go/internal/auth"
)

func TestSendMessagePreservesSuppliedIdentity(t *testing.T) {
	received := make(chan WireMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/sendmessage" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Message WireMessage `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received <- body.Message
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	result, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
		ClientID: "client-fixed",
		RunID:    "run-1",
		Item:     MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if result != (SendResult{ClientID: "client-fixed", RunID: "run-1"}) {
		t.Fatalf("result = %+v", result)
	}
	wire := <-received
	if wire.ClientID != result.ClientID || wire.RunID != result.RunID {
		t.Fatalf("wire identity = client %q run %q", wire.ClientID, wire.RunID)
	}
	if wire.ToUserID != "user-1" || wire.ContextToken != "context-1" {
		t.Fatalf("wire routing = to %q context %q", wire.ToUserID, wire.ContextToken)
	}
	if len(wire.ItemList) != 1 || wire.ItemList[0].TextItem == nil || wire.ItemList[0].TextItem.Text != "hello" {
		t.Fatalf("wire items = %+v", wire.ItemList)
	}
}

func TestSendMessageReturnsGeneratedIdentityOnFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	result, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
		RunID: "run-1",
		Item:  MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
	})
	if err == nil {
		t.Fatal("expected send error")
	}
	if result.ClientID == "" || result.RunID != "run-1" {
		t.Fatalf("result after failure = %+v", result)
	}
}

func TestSendMessageExplicitRetryReusesGeneratedIdentity(t *testing.T) {
	clientIDs := make(chan string, 2)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message WireMessage `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		clientIDs <- body.Message.ClientID
		if attempts.Add(1) == 1 {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	message := OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}}}
	first, err := bot.SendMessage(context.Background(), "user-1", message)
	if err == nil || first.ClientID == "" {
		t.Fatalf("first send = result %+v, error %v", first, err)
	}
	message.ClientID = first.ClientID
	second, err := bot.SendMessage(context.Background(), "user-1", message)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.ClientID != first.ClientID {
		t.Fatalf("retry result client ID = %q, want %q", second.ClientID, first.ClientID)
	}
	firstWireID, secondWireID := <-clientIDs, <-clientIDs
	if firstWireID != first.ClientID || secondWireID != first.ClientID {
		t.Fatalf("wire client IDs = %q, %q; want %q", firstWireID, secondWireID, first.ClientID)
	}
}

func TestSendMessageHooksWrapWireAttempt(t *testing.T) {
	received := make(chan WireMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message WireMessage `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received <- body.Message
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	var afterCalls atomic.Int32
	bot := newOutboundTestBot(t, server)
	bot.Hooks().BeforeSend.Register(func(req *SendRequest) error {
		if req.Message.ClientID == "" {
			t.Fatal("BeforeSend saw an empty generated client ID")
		}
		req.Message.RunID = "run-from-hook"
		req.Message.Item.TextItem.Text = "hooked"
		return nil
	})
	bot.Hooks().AfterSend.Register(func(outcome SendOutcome) error {
		afterCalls.Add(1)
		if outcome.Err != nil {
			t.Errorf("AfterSend error = %v", outcome.Err)
		}
		if outcome.Result.ClientID == "" || outcome.Result.RunID != "run-from-hook" {
			t.Errorf("AfterSend result = %+v", outcome.Result)
		}
		return nil
	})

	result, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
		Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "original"}},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	wire := <-received
	if wire.ClientID != result.ClientID || wire.RunID != "run-from-hook" || wire.ItemList[0].TextItem.Text != "hooked" {
		t.Fatalf("wire after hook = %+v", wire)
	}
	if afterCalls.Load() != 1 {
		t.Fatalf("AfterSend calls = %d", afterCalls.Load())
	}
}

func TestBeforeSendFailurePreventsHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	bot.Hooks().BeforeSend.Register(func(*SendRequest) error {
		return errors.New("blocked")
	})
	result, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
		Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
	})
	if err == nil || !strings.Contains(err.Error(), "BeforeSend hook failed") {
		t.Fatalf("error = %v", err)
	}
	if result.ClientID == "" {
		t.Fatal("generated client ID was not returned")
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d", requests.Load())
	}
}

func TestBeforeSendCannotChangeIdentityOrDestination(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SendRequest)
		want   string
	}{
		{name: "client ID", mutate: func(req *SendRequest) { req.Message.ClientID = "changed" }, want: "client ID"},
		{name: "user ID", mutate: func(req *SendRequest) { req.UserID = "other-user" }, want: "user ID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
			}))
			defer server.Close()

			bot := newOutboundTestBot(t, server)
			bot.Hooks().BeforeSend.Register(func(req *SendRequest) error {
				tc.mutate(req)
				return nil
			})
			result, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
				ClientID: "client-fixed",
				Item:     MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			if result.ClientID != "client-fixed" {
				t.Fatalf("result = %+v", result)
			}
			if requests.Load() != 0 {
				t.Fatalf("HTTP requests = %d", requests.Load())
			}
		})
	}
}

func TestAfterSendObservesFailureAndCannotOverrideSuccess(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "failed", http.StatusBadGateway)
		}))
		defer server.Close()

		bot := newOutboundTestBot(t, server)
		var observed error
		bot.Hooks().AfterSend.Register(func(outcome SendOutcome) error {
			observed = outcome.Err
			return nil
		})
		_, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
			Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		})
		if err == nil || observed == nil {
			t.Fatalf("send error = %v, observed = %v", err, observed)
		}
	})

	t.Run("after hook error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		}))
		defer server.Close()

		var reported atomic.Int32
		bot := newOutboundTestBot(t, server)
		bot.opts.LogLevel = "silent"
		bot.opts.OnError = func(error) { reported.Add(1) }
		bot.Hooks().AfterSend.Register(func(SendOutcome) error {
			return errors.New("observer failed")
		})
		_, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
			Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		})
		if err != nil {
			t.Fatalf("AfterSend error changed send result: %v", err)
		}
		if reported.Load() != 1 {
			t.Fatalf("reported errors = %d", reported.Load())
		}
	})
}

func TestSendTextChunksUseDistinctClientIDs(t *testing.T) {
	clientIDs := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message WireMessage `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		clientIDs <- body.Message.ClientID
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	if err := bot.Send(context.Background(), "user-1", strings.Repeat("x", maxTextChars+1)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	first, second := <-clientIDs, <-clientIDs
	if first == "" || second == "" || first == second {
		t.Fatalf("chunk client IDs = %q, %q", first, second)
	}
}

func TestSendMediaHooksEachWireMessageOnce(t *testing.T) {
	messages := make(chan WireMessage, 2)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ret":             0,
				"upload_full_url": server.URL + "/upload",
			})
		case "/upload":
			w.Header().Set("x-encrypted-param", "encrypted-main")
			w.WriteHeader(http.StatusOK)
		case "/ilink/bot/sendmessage":
			var body struct {
				Message WireMessage `json:"msg"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}
			messages <- body.Message
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	var beforeCalls atomic.Int32
	var afterCalls atomic.Int32
	bot.Hooks().BeforeSend.Register(func(*SendRequest) error {
		beforeCalls.Add(1)
		return nil
	})
	bot.Hooks().AfterSend.Register(func(SendOutcome) error {
		afterCalls.Add(1)
		return nil
	})
	if err := bot.SendMedia(context.Background(), "user-1", SendContent{
		Caption: "caption",
		Image:   []byte("image bytes"),
	}); err != nil {
		t.Fatalf("SendMedia: %v", err)
	}

	caption, image := <-messages, <-messages
	if len(caption.ItemList) != 1 || caption.ItemList[0].Type != ItemText {
		t.Fatalf("caption wire = %+v", caption)
	}
	if len(image.ItemList) != 1 || image.ItemList[0].Type != ItemImage || image.ItemList[0].ImageItem == nil {
		t.Fatalf("image wire = %+v", image)
	}
	if caption.ClientID == "" || image.ClientID == "" || caption.ClientID == image.ClientID {
		t.Fatalf("media client IDs = %q, %q", caption.ClientID, image.ClientID)
	}
	if beforeCalls.Load() != 2 || afterCalls.Load() != 2 {
		t.Fatalf("hook calls = before %d after %d", beforeCalls.Load(), afterCalls.Load())
	}
}

func TestReplyMessageDoesNotInheritInboundRunID(t *testing.T) {
	received := make(chan WireMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message WireMessage `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received <- body.Message
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	bot := newOutboundTestBot(t, server)
	result, err := bot.ReplyMessage(context.Background(), &IncomingMessage{
		UserID:       "user-1",
		ContextToken: "reply-context",
		RunID:        "inbound-run",
	}, OutboundMessage{Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}}})
	if err != nil {
		t.Fatalf("ReplyMessage: %v", err)
	}
	wire := <-received
	if result.RunID != "" || wire.RunID != "" {
		t.Fatalf("inherited run ID: result %q wire %q", result.RunID, wire.RunID)
	}
	if wire.ContextToken != "reply-context" {
		t.Fatalf("context token = %q", wire.ContextToken)
	}
}

func TestToolCallMessageBuilders(t *testing.T) {
	start := NewToolCallStartMessage("run-1", "search", "call-1")
	if start.RunID != "run-1" || start.Item.Type != ItemToolCallStart || start.Item.CreateTimeMs == 0 || start.Item.IsCompleted == nil || *start.Item.IsCompleted {
		t.Fatalf("start message = %+v", start)
	}
	if start.Item.ToolCallStartItem == nil || start.Item.ToolCallStartItem.ToolName != "search" || start.Item.ToolCallStartItem.ToolCallID != "call-1" {
		t.Fatalf("start item = %+v", start.Item.ToolCallStartItem)
	}

	result := NewToolCallResultMessage("run-1", "search", "call-1", "unexpected")
	if result.Item.Type != ItemToolCallResult || result.Item.IsCompleted == nil || !*result.Item.IsCompleted {
		t.Fatalf("result message = %+v", result)
	}
	if result.Item.ToolCallResultItem == nil || result.Item.ToolCallResultItem.Status != string(ToolCallUnknown) {
		t.Fatalf("result item = %+v", result.Item.ToolCallResultItem)
	}
}

func newOutboundTestBot(t *testing.T, server *httptest.Server) *Bot {
	t.Helper()
	bot := New(Options{
		ContextTokenPath: filepath.Join(t.TempDir(), "context_tokens.json"),
		LogLevel:         "silent",
	})
	bot.client.HTTP = server.Client()
	bot.creds = &auth.Credentials{BaseURL: server.URL, Token: "token"}
	if err := bot.contextTokens.Set("user-1", "context-1"); err != nil {
		t.Fatalf("set context token: %v", err)
	}
	return bot
}
