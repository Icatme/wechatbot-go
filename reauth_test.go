package wechatbot

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/store"
)

type sessionTestPaths struct {
	credentials string
	context     string
	cursor      string
	replay      string
}

func TestReauthenticationTransitionClearsSessionAndPreservesDeliveryState(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://api.example")
	invalidGeneration := bot.sessionGeneration
	if err := bot.cursorStore.Set("cursor-1"); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	if err := bot.replayStore.Commit("message:1"); err != nil {
		t.Fatalf("commit replay: %v", err)
	}

	cause := fmt.Errorf("poll failed: %w", &APIError{
		Endpoint: "/ilink/bot/getupdates",
		RetCode:  -14,
		Message:  "expired",
	})
	err := bot.handleAuthenticatedError(cause)
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("error = %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetCode != -14 {
		t.Fatalf("lost API error: %T %v", err, err)
	}
	if !bot.ReauthRequired() {
		t.Fatal("reauthentication state was not published")
	}
	if err := bot.handleAuthenticatedErrorForSession(invalidGeneration, "invalid-token", nil); err != nil {
		t.Fatalf("a successful concurrent request was changed into a failure: %v", err)
	}

	bot.mu.Lock()
	creds := bot.creds
	configCache := bot.configCache
	bot.mu.Unlock()
	if creds != nil || configCache != nil {
		t.Fatalf("stale session remains: credentials=%+v config=%p", creds, configCache)
	}
	if got := bot.contextTokens.Get("user-1"); got != "" {
		t.Fatalf("context token survived invalidation: %q", got)
	}
	if _, statErr := os.Stat(paths.credentials); !os.IsNotExist(statErr) {
		t.Fatalf("credentials file still exists: %v", statErr)
	}
	if got := bot.cursorStore.Get(); got != "cursor-1" {
		t.Fatalf("cursor was cleared: %q", got)
	}
	if !bot.replayStore.Seen("message:1") {
		t.Fatal("replay state was cleared")
	}
	if _, statErr := os.Stat(paths.cursor); statErr != nil {
		t.Fatalf("cursor file was removed: %v", statErr)
	}
	if _, statErr := os.Stat(paths.replay); statErr != nil {
		t.Fatalf("replay file was removed: %v", statErr)
	}
}

func TestConcurrentSessionExpirySharesOneTerminalError(t *testing.T) {
	bot, _ := newAuthenticatedSessionTestBot(t, "https://api.example")
	cause := &APIError{Endpoint: "/ilink/bot/sendmessage", ErrCode: -14, Message: "expired"}

	const callers = 16
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- bot.handleAuthenticatedError(cause)
		}()
	}
	wg.Wait()
	close(errs)

	var first *ReauthRequiredError
	for err := range errs {
		if !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("error = %v", err)
		}
		var current *ReauthRequiredError
		if !errors.As(err, &current) {
			t.Fatalf("error type = %T", err)
		}
		if first == nil {
			first = current
		} else if current != first {
			t.Fatalf("concurrent transition returned distinct errors: %p and %p", first, current)
		}
	}
}

func TestRunReturnsReauthRequiredWithoutRetryingExpiredSession(t *testing.T) {
	var updates atomic.Int32
	var qrRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart":
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/getupdates":
			updates.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ret":    -14,
				"errmsg": "expired",
			})
		case "/ilink/bot/get_bot_qrcode", "/ilink/bot/get_qrcode_status":
			qrRequests.Add(1)
			http.Error(w, "unexpected QR request", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = server.Client()
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}))

	started := time.Now()
	err := bot.Run(context.Background())
	if !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("Run error = %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Endpoint != "/ilink/bot/getupdates" {
		t.Fatalf("Run lost API error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Run waited instead of terminating: %v", elapsed)
	}
	if updates.Load() != 1 {
		t.Fatalf("getupdates calls = %d", updates.Load())
	}
	if qrRequests.Load() != 0 {
		t.Fatalf("automatic QR requests = %d", qrRequests.Load())
	}
}

func TestRunPreservesHandlerRetryWhenSendTriggersReauthentication(t *testing.T) {
	handlerErr := errors.New("handler retry")
	releaseUpdates := make(chan struct{})
	var polls atomic.Int32
	raw := marshalWireMessage(t, WireMessage{
		MessageID:    101,
		FromUserID:   "user-1",
		ToUserID:     "bot-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "context-from-message",
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		},
	})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart":
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/getupdates":
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ret":             0,
					"msgs":            []json.RawMessage{raw},
					"get_updates_buf": "cursor-1",
				})
				return
			}
			select {
			case <-r.Context().Done():
			case <-releaseUpdates:
			}
		case "/ilink/bot/sendmessage":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ret":    -14,
				"errmsg": "expired",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer close(releaseUpdates)

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = server.Client()
	bot.Handle(MessageHandlerFunc(func(ctx context.Context, msg *IncomingMessage) MessageResult {
		_, sendErr := bot.SendMessage(ctx, msg.UserID, OutboundMessage{
			Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "reply"}},
		})
		if !errors.Is(sendErr, ErrReauthRequired) {
			t.Errorf("send error = %v", sendErr)
		}
		return RetryMessage(handlerErr)
	}))

	err := bot.Run(context.Background())
	if !errors.Is(err, ErrReauthRequired) || !errors.Is(err, handlerErr) {
		t.Fatalf("Run error = %v", err)
	}
	if bot.replayStore.Seen(reauthReplayKey(101, "user-1")) {
		t.Fatal("retrying message was committed")
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("retrying batch cursor committed: %q", got)
	}
}

func TestOutboundSessionExpiryBlocksLaterCallsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"ret":     0,
			"errcode": -14,
			"errmsg":  "expired",
		})
	}))
	defer server.Close()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = server.Client()
	result, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
		Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
	})
	if !errors.Is(err, ErrReauthRequired) || result.ClientID == "" {
		t.Fatalf("first send = result %+v error %v", result, err)
	}
	if _, err := bot.SendMessage(context.Background(), "user-1", OutboundMessage{
		Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "again"}},
	}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("second send error = %v", err)
	}
	if err := bot.StopTyping(context.Background(), "user-1"); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("StopTyping error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("network requests after invalidation = %d", requests.Load())
	}
}

func TestAuthenticatedHelpersEnterReauthState(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Bot) error
	}{
		{
			name: "get config",
			call: func(ctx context.Context, bot *Bot) error {
				return bot.SendTyping(ctx, "user-1")
			},
		},
		{
			name: "get upload URL",
			call: func(ctx context.Context, bot *Bot) error {
				_, err := bot.Upload(ctx, []byte("file"), "user-1", int(MediaFile))
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ret":    -14,
					"errmsg": "expired",
				})
			}))
			defer server.Close()

			bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
			bot.client.HTTP = server.Client()
			if err := tc.call(context.Background(), bot); !errors.Is(err, ErrReauthRequired) {
				t.Fatalf("error = %v", err)
			}
			if !bot.ReauthRequired() || requests.Load() != 1 {
				t.Fatalf("state=%v requests=%d", bot.ReauthRequired(), requests.Load())
			}
		})
	}
}

func TestNotifyStopSessionExpiryChangesRunResult(t *testing.T) {
	getUpdatesStarted := make(chan struct{}, 1)
	releaseUpdates := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart":
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/getupdates":
			getUpdatesStarted <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-releaseUpdates:
			}
		case "/ilink/bot/msg/notifystop":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ret":    -14,
				"errmsg": "expired on stop",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer close(releaseUpdates)

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = server.Client()
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- bot.Run(ctx) }()
	waitForSignal(t, getUpdatesStarted, "getupdates")
	cancel()

	select {
	case err := <-runDone:
		if !errors.Is(err, ErrReauthRequired) {
			t.Fatalf("Run error = %v", err)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Endpoint != "/ilink/bot/msg/notifystop" {
			t.Fatalf("NotifyStop API error missing: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestNotifyStartSessionExpiryStopsBeforePolling(t *testing.T) {
	var notifyStartCalls atomic.Int32
	var pollCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart":
			notifyStartCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ret": -14, "errmsg": "expired"})
		case "/ilink/bot/getupdates":
			pollCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = server.Client()
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}))
	if err := bot.Run(context.Background()); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("Run error = %v", err)
	}
	if notifyStartCalls.Load() != 1 || pollCalls.Load() != 0 {
		t.Fatalf("NotifyStart calls = %d, poll calls = %d", notifyStartCalls.Load(), pollCalls.Load())
	}
}

func TestSendTypingSessionExpiryTransitionsAfterTicketFetch(t *testing.T) {
	var getConfigCalls atomic.Int32
	var sendTypingCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getconfig":
			getConfigCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ret": 0, "typing_ticket": "ticket-1"})
		case "/ilink/bot/sendtyping":
			sendTypingCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ret": -14, "errmsg": "expired"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = server.Client()
	if err := bot.SendTyping(context.Background(), "user-1"); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("SendTyping error = %v", err)
	}
	if getConfigCalls.Load() != 1 || sendTypingCalls.Load() != 1 {
		t.Fatalf("getconfig calls = %d, sendtyping calls = %d", getConfigCalls.Load(), sendTypingCalls.Load())
	}
}

func TestExplicitReauthenticationUsesFreshCredentialsAndHooks(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://old.example")
	invalidGeneration := bot.sessionGeneration
	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate: %v", err)
	}

	var requests atomic.Int32
	var qrBody []byte
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		var body string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if req.Body != nil {
				qrBody, _ = io.ReadAll(req.Body)
			}
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			body = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	if _, err := bot.Login(context.Background(), false); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("non-force Login error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("non-force Login made %d requests", requests.Load())
	}

	var hookMu sync.Mutex
	var hooks []string
	var beforePayload *Credentials
	var afterPayload *Credentials
	bot.Hooks().BeforeLogin.Register(func(creds *Credentials) error {
		beforePayload = creds
		hookMu.Lock()
		hooks = append(hooks, "before:"+creds.Token)
		hookMu.Unlock()
		if _, err := bot.Reauthenticate(context.Background()); !errors.Is(err, ErrLoginInProgress) {
			return fmt.Errorf("reentrant Reauthenticate error = %v", err)
		}
		if _, err := bot.readyCreds(); !errors.Is(err, ErrReauthRequired) {
			return fmt.Errorf("credentials became ready before installation: %v", err)
		}
		creds.Token = "mutated-by-before-hook"
		return nil
	})
	bot.Hooks().AfterLogin.Register(func(creds *Credentials) error {
		afterPayload = creds
		hookMu.Lock()
		hooks = append(hooks, "after:"+creds.Token)
		hookMu.Unlock()
		installed, err := bot.Login(context.Background(), false)
		if err != nil || installed.Token != "fresh-token" {
			return fmt.Errorf("reentrant Login = %+v, %v", installed, err)
		}
		if ready, err := bot.readyCreds(); err != nil || ready.Token != "fresh-token" {
			return fmt.Errorf("credentials not ready in AfterLogin = %+v, %v", ready, err)
		}
		return nil
	})
	creds, err := bot.Reauthenticate(context.Background())
	if err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	if creds.Token != "fresh-token" || creds.AccountID != "bot-1" || creds.BaseURL != "https://fresh.example" {
		t.Fatalf("fresh credentials = %+v", creds)
	}
	if beforePayload == afterPayload || beforePayload == creds || afterPayload == creds {
		t.Fatal("login hooks and caller shared a credentials payload")
	}
	if bot.ReauthRequired() {
		t.Fatal("reauthentication state was not cleared")
	}
	if requests.Load() != 2 {
		t.Fatalf("reauth requests = %d", requests.Load())
	}
	if len(qrBody) != 0 {
		var request struct {
			LocalTokens []string `json:"local_token_list"`
		}
		if err := json.Unmarshal(qrBody, &request); err != nil {
			t.Fatalf("decode QR body: %v", err)
		}
		if len(request.LocalTokens) != 0 {
			t.Fatalf("invalid token offered for reauth: %v", request.LocalTokens)
		}
	}
	if got := bot.contextTokens.Get("user-1"); got != "" {
		t.Fatalf("old context restored: %q", got)
	}
	stored, err := auth.LoadCredentials(paths.credentials)
	if err != nil || stored == nil || stored.Token != "fresh-token" {
		t.Fatalf("stored credentials = %+v, %v", stored, err)
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	wantHooks := []string{"before:fresh-token", "after:fresh-token"}
	if fmt.Sprint(hooks) != fmt.Sprint(wantHooks) {
		t.Fatalf("hooks = %v, want %v", hooks, wantHooks)
	}

	staleErr := bot.handleAuthenticatedErrorForSession(invalidGeneration, "invalid-token", &APIError{
		RetCode: -14,
		Message: "late response from old token",
	})
	if !errors.Is(staleErr, ErrSessionChanged) || errors.Is(staleErr, ErrReauthRequired) || bot.ReauthRequired() {
		t.Fatalf("late old-token response invalidated fresh credentials: %v", staleErr)
	}
	ready, err := bot.readyCreds()
	if err != nil || ready.Token != "fresh-token" {
		t.Fatalf("fresh credentials lost: %+v, %v", ready, err)
	}
}

func TestHealthyReauthenticationGatesOldSessionAndPreservesStores(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://old.example")
	oldContextStore := bot.contextTokens
	oldCursorStore := bot.cursorStore
	oldReplayStore := bot.replayStore
	if err := oldCursorStore.Set("cursor-before-reauth"); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	if err := oldReplayStore.Commit("message:before-reauth"); err != nil {
		t.Fatalf("set replay key: %v", err)
	}

	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if _, err := bot.readyCreds(); !errors.Is(err, ErrReauthRequired) {
				return nil, fmt.Errorf("old session remained ready during QR: %v", err)
			}
			if got := bot.contextTokens.Get("user-1"); got != "" {
				return nil, fmt.Errorf("old context remained during QR: %q", got)
			}
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			body = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	creds, err := bot.Reauthenticate(context.Background())
	if err != nil {
		t.Fatalf("Reauthenticate healthy session: %v", err)
	}
	if creds.Token != "fresh-token" || bot.ReauthRequired() {
		t.Fatalf("fresh session = %+v, required = %v", creds, bot.ReauthRequired())
	}
	if bot.contextTokens != oldContextStore || bot.cursorStore != oldCursorStore || bot.replayStore != oldReplayStore {
		t.Fatal("healthy reauthentication replaced delivery stores")
	}
	if got := bot.cursorStore.Get(); got != "cursor-before-reauth" {
		t.Fatalf("cursor changed: %q", got)
	}
	if !bot.replayStore.Seen("message:before-reauth") {
		t.Fatal("replay state changed")
	}
	stored, err := auth.LoadCredentials(paths.credentials)
	if err != nil || stored == nil || stored.Token != "fresh-token" {
		t.Fatalf("stored fresh credentials = %+v, %v", stored, err)
	}
}

func TestHealthyReauthenticationAccountMismatchStaysFailClosed(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://old.example")
	oldContextStore := bot.contextTokens
	oldCursorStore := bot.cursorStore
	oldReplayStore := bot.replayStore
	var hookCalls atomic.Int32
	bot.Hooks().BeforeLogin.Register(func(*Credentials) error {
		hookCalls.Add(1)
		return nil
	})
	bot.Hooks().AfterLogin.Register(func(*Credentials) error {
		hookCalls.Add(1)
		return nil
	})
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		if req.URL.Path == "/ilink/bot/get_qrcode_status" {
			body = `{"status":"confirmed","bot_token":"other-token","ilink_bot_id":"other-bot","ilink_user_id":"user-2","baseurl":"https://fresh.example"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	if _, err := bot.Reauthenticate(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("account mismatch error = %v", err)
	}
	if !bot.ReauthRequired() {
		t.Fatal("account mismatch reopened the old session")
	}
	if _, err := bot.readyCreds(); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("ready credentials after mismatch = %v", err)
	}
	if _, err := os.Stat(paths.credentials); !os.IsNotExist(err) {
		t.Fatalf("mismatched candidate credentials remain: %v", err)
	}
	if hookCalls.Load() != 0 {
		t.Fatalf("login hooks ran for mismatched account: %d", hookCalls.Load())
	}
	if bot.contextTokens != oldContextStore || bot.cursorStore != oldCursorStore || bot.replayStore != oldReplayStore {
		t.Fatal("account mismatch replaced delivery stores")
	}
}

func TestReauthenticationRejectsInvalidatedTokenAtBotBoundary(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://old.example")
	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate: %v", err)
	}
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		if req.URL.Path == "/ilink/bot/get_qrcode_status" {
			body = `{"status":"confirmed","bot_token":"invalid-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	_, err := bot.Reauthenticate(context.Background())
	if !errors.Is(err, auth.ErrInvalidatedCredential) || !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("same-token reauthentication error = %v", err)
	}
	if !bot.ReauthRequired() {
		t.Fatal("same-token response reopened the session")
	}
	if _, err := os.Stat(paths.credentials); !os.IsNotExist(err) {
		t.Fatalf("invalid token was persisted: %v", err)
	}
}

func TestLateExpiryCannotInvalidateReusedTokenFromNewGeneration(t *testing.T) {
	bot, _ := newAuthenticatedSessionTestBot(t, "https://old.example")
	oldGeneration := bot.sessionGeneration
	var confirmations atomic.Int32
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		if req.URL.Path == "/ilink/bot/get_qrcode_status" {
			token := "fresh-token"
			if confirmations.Add(1) == 2 {
				token = "invalid-token"
			}
			body = fmt.Sprintf(`{"status":"confirmed","bot_token":%q,"ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`, token)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	if creds, err := bot.Reauthenticate(context.Background()); err != nil || creds.Token != "fresh-token" {
		t.Fatalf("first Reauthenticate = %+v, %v", creds, err)
	}
	if creds, err := bot.Reauthenticate(context.Background()); err != nil || creds.Token != "invalid-token" {
		t.Fatalf("second Reauthenticate = %+v, %v", creds, err)
	}
	lateErr := bot.handleAuthenticatedErrorForSession(oldGeneration, "invalid-token", &APIError{RetCode: -14, Message: "late gen-0 response"})
	if !errors.Is(lateErr, ErrSessionChanged) || errors.Is(lateErr, ErrReauthRequired) {
		t.Fatalf("late old-generation response = %v", lateErr)
	}
	ready, err := bot.readyCreds()
	if err != nil || ready.Token != "invalid-token" || bot.ReauthRequired() {
		t.Fatalf("current reused-token session = %+v, %v, required=%v", ready, err, bot.ReauthRequired())
	}
}

func TestStaleContextWriteCannotCrossSessionGeneration(t *testing.T) {
	bot, _ := newAuthenticatedSessionTestBot(t, "https://old.example")
	oldGeneration := bot.sessionGeneration
	bot.sessionMu.Lock()
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		done <- bot.persistContextToken(oldGeneration, "stale-user", "stale-context")
	}()
	<-started
	bot.mu.Lock()
	bot.sessionGeneration++
	bot.mu.Unlock()
	bot.sessionMu.Unlock()

	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("stale context write error = %v", err)
	}
	if got := bot.contextTokens.Get("stale-user"); got != "" {
		t.Fatalf("stale context was restored: %q", got)
	}
}

func TestQueuedOldDeliveryStopsBeforeReplayLookupOrHandler(t *testing.T) {
	bot, _ := newAuthenticatedSessionTestBot(t, "https://old.example")
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		t.Fatal("stale delivery reached handler")
		return AckMessage()
	}))
	session, _, _, cancel, err := bot.beginRun(context.Background())
	if err != nil {
		t.Fatalf("beginRun: %v", err)
	}
	bot.finishRun(cancel)
	bot.client.HTTP = reauthenticationTestClient(rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
	}), "fresh-token", "bot-1", "https://fresh.example")
	if _, err := bot.Reauthenticate(context.Background()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	wire := &WireMessage{
		MessageID:    601,
		FromUserID:   "user-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ItemList:     []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "stale"}}},
	}
	err = bot.processWireMessage(context.Background(), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		t.Fatal("stale delivery reached direct handler")
		return AckMessage()
	}), session.generation, wire)
	if !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("stale delivery error = %v", err)
	}
}

func TestTypingDoesNotStartWithOldSessionAfterReauthentication(t *testing.T) {
	getConfigStarted := make(chan struct{})
	releaseConfig := make(chan struct{})
	var releaseConfigOnce sync.Once
	releaseConfigNow := func() { releaseConfigOnce.Do(func() { close(releaseConfig) }) }
	var sendTypingCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getconfig":
			close(getConfigStarted)
			<-releaseConfig
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ret": 0, "typing_ticket": "old-ticket"})
		case "/ilink/bot/sendtyping":
			sendTypingCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer releaseConfigNow()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	serverTransport := server.Client().Transport
	bot.client.HTTP = reauthenticationTestClient(serverTransport, "fresh-token", "bot-1", server.URL)
	typingDone := make(chan error, 1)
	go func() { typingDone <- bot.SendTyping(context.Background(), "user-1") }()
	waitForSignal(t, getConfigStarted, "getconfig")
	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate: %v", err)
	}
	reauthDone := startTestReauthentication(bot)
	if result := waitForTestReauthentication(t, reauthDone); result.err != nil {
		t.Fatalf("Reauthenticate: %v", result.err)
	}
	releaseConfigNow()
	if err := <-typingDone; !isExpectedOldRequestResult(err) {
		t.Fatalf("old SendTyping error = %v", err)
	}
	if sendTypingCalls.Load() != 0 {
		t.Fatalf("old session started %d sendtyping requests after reauthentication", sendTypingCalls.Load())
	}
}

func TestOldConfigCacheRejectsChangedSessionBeforeGetConfig(t *testing.T) {
	var getConfigCalls atomic.Int32
	bot, _ := newAuthenticatedSessionTestBot(t, "https://old.example")
	bot.mu.Lock()
	oldConfigCache := bot.configCache
	bot.mu.Unlock()
	bot.client.HTTP = reauthenticationTestClient(rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/ilink/bot/getconfig" {
			getConfigCalls.Add(1)
		}
		return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
	}), "fresh-token", "bot-1", "https://fresh.example")

	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := bot.Reauthenticate(context.Background()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	if _, err := oldConfigCache.GetForUser(context.Background(), "user-1", "context-1"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("old config cache error = %v", err)
	}
	if getConfigCalls.Load() != 0 {
		t.Fatalf("old config cache started %d getconfig requests after reauthentication", getConfigCalls.Load())
	}
}

func TestTypingEmptyCachedTicketRejectsChangedSession(t *testing.T) {
	operations := map[string]func(*Bot) error{
		"send": func(bot *Bot) error {
			return bot.SendTyping(context.Background(), "user-1")
		},
		"stop": func(bot *Bot) error {
			return bot.StopTyping(context.Background(), "user-1")
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			getConfigStarted := make(chan struct{})
			releaseConfig := make(chan struct{})
			var releaseConfigOnce sync.Once
			releaseConfigNow := func() { releaseConfigOnce.Do(func() { close(releaseConfig) }) }
			var sendTypingCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ilink/bot/getconfig":
					close(getConfigStarted)
					<-releaseConfig
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"ret": 0, "typing_ticket": ""})
				case "/ilink/bot/sendtyping":
					sendTypingCalls.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			defer releaseConfigNow()

			bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
			bot.client.HTTP = reauthenticationTestClient(server.Client().Transport, "fresh-token", "bot-1", server.URL)
			done := make(chan error, 1)
			go func() { done <- operation(bot) }()
			waitForSignal(t, getConfigStarted, "getconfig")
			if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
				t.Fatalf("invalidate: %v", err)
			}
			reauthDone := startTestReauthentication(bot)
			if result := waitForTestReauthentication(t, reauthDone); result.err != nil {
				t.Fatalf("Reauthenticate: %v", result.err)
			}
			releaseConfigNow()

			if err := <-done; !isExpectedOldRequestResult(err) {
				t.Fatalf("old %s typing with empty ticket error = %v", name, err)
			}
			if sendTypingCalls.Load() != 0 {
				t.Fatalf("old session started %d sendtyping requests after reauthentication", sendTypingCalls.Load())
			}
		})
	}
}

func TestAuthenticatedRequestGatePreventsOldTokenAfterFreshInstall(t *testing.T) {
	tests := []struct {
		name         string
		typingTicket string
		targetPath   string
		primeConfig  bool
		upload       bool
		blockRead    int32
	}{
		{
			name:       "getconfig",
			targetPath: "/ilink/bot/getconfig",
			blockRead:  1,
		},
		{
			name:         "sendtyping",
			typingTicket: "old-ticket",
			targetPath:   "/ilink/bot/sendtyping",
			primeConfig:  true,
			blockRead:    1,
		},
		{
			name:       "getuploadurl",
			targetPath: "/ilink/bot/getuploadurl",
			upload:     true,
			blockRead:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bot *Bot
			var oldGeneration uint64
			var targetCalls atomic.Int32
			var oldTokenRequestsAfterFreshInstall atomic.Int32
			baseTransport := rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Authorization") == "Bearer invalid-token" {
					bot.mu.Lock()
					generation := bot.sessionGeneration
					bot.mu.Unlock()
					if generation != oldGeneration {
						oldTokenRequestsAfterFreshInstall.Add(1)
					}
				}
				if req.URL.Path == tt.targetPath {
					targetCalls.Add(1)
				}
				var body string
				header := make(http.Header)
				switch req.URL.Path {
				case "/ilink/bot/getconfig":
					body = fmt.Sprintf(`{"ret":0,"typing_ticket":%q}`, tt.typingTicket)
				case "/ilink/bot/sendtyping":
					body = `{"ret":0}`
				case "/ilink/bot/getuploadurl":
					body = `{"ret":0,"upload_full_url":"https://old.example/cdn-upload"}`
				case "/cdn-upload":
					header.Set("x-encrypted-param", "download-param")
				default:
					return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     header,
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			})

			bot, _ = newAuthenticatedSessionTestBot(t, "https://old.example")
			oldGeneration = bot.sessionGeneration
			bot.client.HTTP = reauthenticationTestClient(baseTransport, "fresh-token", "bot-1", "https://fresh.example")
			if tt.primeConfig {
				bot.mu.Lock()
				configCache := bot.configCache
				bot.mu.Unlock()
				if _, err := configCache.GetForUser(context.Background(), "user-1", "context-1"); err != nil {
					t.Fatalf("prime config cache: %v", err)
				}
			}

			beforeFreshInstall := make(chan struct{})
			bot.Hooks().BeforeLogin.Register(func(creds *Credentials) error {
				if creds.Token == "fresh-token" {
					close(beforeFreshInstall)
				}
				return nil
			})
			originalReader := cryptorand.Reader
			requestPreparing := make(chan struct{})
			releaseRequest := make(chan struct{})
			var releaseRequestOnce sync.Once
			releaseRequestNow := func() { releaseRequestOnce.Do(func() { close(releaseRequest) }) }
			cryptorand.Reader = &blockingNthRandomReader{
				delegate: originalReader,
				started:  requestPreparing,
				release:  releaseRequest,
				blockAt:  tt.blockRead,
			}
			defer func() {
				releaseRequestNow()
				cryptorand.Reader = originalReader
			}()

			requestDone := make(chan error, 1)
			go func() {
				if tt.upload {
					_, err := bot.Upload(context.Background(), []byte("payload"), "user-1", int(MediaFile))
					requestDone <- err
					return
				}
				requestDone <- bot.SendTyping(context.Background(), "user-1")
			}()
			waitForSignal(t, requestPreparing, tt.name+" AuthHeaders")
			if got := targetCalls.Load(); got != 0 {
				t.Fatalf("%s reached HTTP.Do before AuthHeaders was released: %d", tt.name, got)
			}

			reauthDone := startTestReauthentication(bot)
			waitForSignal(t, beforeFreshInstall, "fresh credentials before install")
			waitForReauthenticationWriter(t, bot, reauthDone, "blocked "+tt.name)
			bot.mu.Lock()
			generationBeforeRelease := bot.sessionGeneration
			bot.mu.Unlock()
			if generationBeforeRelease != oldGeneration {
				t.Fatalf("fresh generation installed while old %s request was preparing: %d", tt.name, generationBeforeRelease)
			}

			releaseRequestNow()
			requestErr := waitForTestRequest(t, requestDone, tt.name)
			result := waitForTestReauthentication(t, reauthDone)
			if result.err != nil {
				t.Fatalf("Reauthenticate: %v", result.err)
			}
			if result.creds == nil || result.creds.Token != "fresh-token" {
				t.Fatalf("fresh credentials = %+v", result.creds)
			}
			if !isExpectedOldRequestResult(requestErr) {
				t.Fatalf("old %s request error = %v", tt.name, requestErr)
			}
			targetCallCount := targetCalls.Load()
			if targetCallCount == 0 && requestErr == nil {
				t.Fatalf("old %s request succeeded without reaching HTTP.Do", tt.name)
			}
			if targetCallCount > 1 {
				t.Fatalf("%s HTTP calls = %d, want at most 1", tt.name, targetCallCount)
			}
			if got := oldTokenRequestsAfterFreshInstall.Load(); got != 0 {
				t.Fatalf("old-token HTTP requests started after fresh install: %d", got)
			}
		})
	}
}

func TestUploadRechecksSessionAfterPreparation(t *testing.T) {
	bot, _ := newAuthenticatedSessionTestBot(t, "https://old.example")
	var getUploadURLCalls atomic.Int32
	bot.client.HTTP = reauthenticationTestClient(rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/ilink/bot/getuploadurl" {
			getUploadURLCalls.Add(1)
		}
		return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
	}), "fresh-token", "bot-1", "https://fresh.example")

	originalReader := cryptorand.Reader
	started := make(chan struct{})
	release := make(chan struct{})
	cryptorand.Reader = &blockingRandomReader{
		delegate: originalReader,
		started:  started,
		release:  release,
	}
	defer func() { cryptorand.Reader = originalReader }()

	uploadDone := make(chan error, 1)
	go func() {
		_, err := bot.Upload(context.Background(), []byte("payload"), "user-1", int(MediaFile))
		uploadDone <- err
	}()
	waitForSignal(t, started, "upload key generation")
	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := bot.Reauthenticate(context.Background()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	close(release)
	if err := <-uploadDone; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("old Upload error = %v", err)
	}
	if getUploadURLCalls.Load() != 0 {
		t.Fatalf("old session started %d getuploadurl requests after reauthentication", getUploadURLCalls.Load())
	}
}

func TestOldRunNeverNotifiesStopWithFreshCredentials(t *testing.T) {
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	releasePoll := make(chan struct{})
	secondPollStarted := make(chan struct{})
	var releasePollOnce sync.Once
	releasePollNow := func() { releasePollOnce.Do(func() { close(releasePoll) }) }
	var notifyStopTokensMu sync.Mutex
	var notifyStopTokens []string
	var sendTokens []string
	var polls atomic.Int32
	raw := marshalWireMessage(t, WireMessage{
		MessageID:    501,
		FromUserID:   "user-1",
		ToUserID:     "bot-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "old-context",
		ItemList:     []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hello"}}},
	})

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart":
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/getupdates":
			polls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ret":             0,
				"msgs":            []json.RawMessage{raw},
				"get_updates_buf": "cursor-1",
			})
		case "/ilink/bot/msg/notifystop":
			notifyStopTokensMu.Lock()
			notifyStopTokens = append(notifyStopTokens, r.Header.Get("Authorization"))
			notifyStopTokensMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/sendmessage":
			notifyStopTokensMu.Lock()
			sendTokens = append(sendTokens, r.Header.Get("Authorization"))
			notifyStopTokensMu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/get_bot_qrcode":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"qrcode":             "qr",
				"qrcode_img_content": "https://example.com/qr",
			})
		case "/ilink/bot/get_qrcode_status":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status":        "confirmed",
				"bot_token":     "fresh-token",
				"ilink_bot_id":  "bot-1",
				"ilink_user_id": "user-1",
				"baseurl":       server.URL,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer releasePollNow()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	beforeFreshInstall := make(chan struct{})
	bot.Hooks().BeforeLogin.Register(func(creds *Credentials) error {
		if creds.Token == "fresh-token" {
			close(beforeFreshInstall)
		}
		return nil
	})
	serverTransport := server.Client().Transport
	var transportPolls atomic.Int32
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/ilink/bot/getupdates":
			if transportPolls.Add(1) > 1 {
				close(secondPollStarted)
				<-releasePoll
				body = `{"ret":0}`
				break
			}
			return serverTransport.RoundTrip(req)
		case "/ilink/bot/get_bot_qrcode":
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			body = fmt.Sprintf(`{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":%q}`, server.URL)
		default:
			return serverTransport.RoundTrip(req)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	handlerSendErr := make(chan error, 1)
	bot.Handle(MessageHandlerFunc(func(ctx context.Context, _ *IncomingMessage) MessageResult {
		close(handlerStarted)
		<-releaseHandler
		_, err := bot.SendMessage(ctx, "user-1", OutboundMessage{
			Item: MessageItem{Type: ItemText, TextItem: &TextItem{Text: "stale reply"}},
		})
		handlerSendErr <- err
		return AckMessage()
	}))
	runDone := make(chan error, 1)
	go func() { runDone <- bot.Run(context.Background()) }()
	waitForSignal(t, handlerStarted, "handler")
	waitForSignal(t, secondPollStarted, "second getupdates")
	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate old run: %v", err)
	}
	reauthDone := startTestReauthentication(bot)
	waitForSignal(t, beforeFreshInstall, "fresh credentials before install")
	waitForReauthenticationWriter(t, bot, reauthDone, "blocked second getupdates")
	releasePollNow()
	result := waitForTestReauthentication(t, reauthDone)
	if result.err != nil || result.creds == nil || result.creds.Token != "fresh-token" {
		t.Fatalf("Reauthenticate while old run drains = %+v, %v", result.creds, result.err)
	}
	if err := bot.contextTokens.Set("user-1", "fresh-context"); err != nil {
		t.Fatalf("set fresh context: %v", err)
	}
	close(releaseHandler)
	select {
	case err := <-runDone:
		if !errors.Is(err, ErrSessionChanged) || errors.Is(err, ErrReauthRequired) {
			t.Fatalf("old Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old Run did not finish")
	}
	if err := <-handlerSendErr; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("old handler send error = %v", err)
	}
	if !bot.replayStore.Seen(reauthReplayKey(501, "user-1")) {
		t.Fatal("Acked old handler did not commit replay state")
	}
	if got := bot.cursorStore.Get(); got != "cursor-1" {
		t.Fatalf("Acked old handler cursor = %q", got)
	}
	notifyStopTokensMu.Lock()
	defer notifyStopTokensMu.Unlock()
	for _, token := range notifyStopTokens {
		if token == "Bearer fresh-token" {
			t.Fatalf("old Run sent NotifyStop with fresh credentials: %v", notifyStopTokens)
		}
	}
	for _, token := range sendTokens {
		if token == "Bearer fresh-token" {
			t.Fatalf("old handler sent with fresh credentials: %v", sendTokens)
		}
	}
}

func TestRunNormalizesExpiredSessionAfterConcurrentRecovery(t *testing.T) {
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	var polls atomic.Int32
	raw := marshalWireMessage(t, WireMessage{
		MessageID:    701,
		FromUserID:   "user-1",
		ToUserID:     "bot-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "old-context",
		ItemList:     []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hello"}}},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart":
			_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
		case "/ilink/bot/getupdates":
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"ret":             0,
					"msgs":            []json.RawMessage{raw},
					"get_updates_buf": "cursor-1",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ret": -14, "errmsg": "expired on poll"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	bot, _ := newAuthenticatedSessionTestBot(t, server.URL)
	bot.client.HTTP = reauthenticationTestClient(server.Client().Transport, "fresh-token", "bot-1", server.URL)
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		close(handlerStarted)
		<-releaseHandler
		return AckMessage()
	}))
	runDone := make(chan error, 1)
	go func() { runDone <- bot.Run(context.Background()) }()
	waitForSignal(t, handlerStarted, "handler")
	waitForCondition(t, "poll expiry transition", bot.ReauthRequired)
	if _, err := bot.Reauthenticate(context.Background()); err != nil {
		t.Fatalf("Reauthenticate: %v", err)
	}
	close(releaseHandler)
	select {
	case err := <-runDone:
		if !errors.Is(err, ErrSessionChanged) || errors.Is(err, ErrReauthRequired) {
			t.Fatalf("Run error = %v", err)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.RetCode != -14 || apiErr.Endpoint != "/ilink/bot/getupdates" {
			t.Fatalf("Run lost poll API error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish")
	}
}

func TestCleanupFailureRemainsFailClosedUntilRetrySucceeds(t *testing.T) {
	dir := t.TempDir()
	credentialDir := filepath.Join(dir, "credential-dir")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bot := New(Options{
		AccountID:        "bot-1",
		CredPath:         credentialDir,
		ContextTokenPath: filepath.Join(dir, "context.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay.json"),
	})
	bot.creds = &auth.Credentials{Token: "invalid", AccountID: "bot-1", BaseURL: "https://api.example"}
	err := bot.handleAuthenticatedError(&APIError{ErrCode: -14, Message: "expired"})
	var reauthErr *ReauthRequiredError
	if !errors.As(err, &reauthErr) || reauthErr.CleanupErr == nil {
		t.Fatalf("cleanup error = %v", err)
	}
	if _, readyErr := bot.readyCreds(); !errors.Is(readyErr, ErrReauthRequired) {
		t.Fatalf("ready credentials after cleanup failure = %v", readyErr)
	}
	var requests atomic.Int32
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		body := ""
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			body = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	if _, reauthAttemptErr := bot.Reauthenticate(context.Background()); !errors.Is(reauthAttemptErr, ErrReauthRequired) {
		t.Fatalf("Reauthenticate after cleanup failure = %v", reauthAttemptErr)
	}
	if requests.Load() != 0 {
		t.Fatalf("cleanup failure allowed %d reauth requests", requests.Load())
	}
	if err := os.Remove(filepath.Join(credentialDir, "keep")); err != nil {
		t.Fatalf("repair credential path: %v", err)
	}
	creds, retryErr := bot.Reauthenticate(context.Background())
	if retryErr != nil {
		t.Fatalf("Reauthenticate after repair: %v", retryErr)
	}
	if creds.Token != "fresh-token" || requests.Load() != 2 {
		t.Fatalf("recovered credentials = %+v, requests = %d", creds, requests.Load())
	}
}

func TestDurableReauthenticationMarkerBlocksLoginAndRecoversAllContextPaths(t *testing.T) {
	dir := t.TempDir()
	paths := sessionTestPaths{
		credentials: filepath.Join(dir, "credentials.json"),
		context:     filepath.Join(dir, "current-context.json"),
		cursor:      filepath.Join(dir, "cursor.json"),
		replay:      filepath.Join(dir, "replay.json"),
	}
	recordedContextPath := filepath.Join(dir, "recorded-context.json")
	if err := auth.SaveCredentials(&auth.Credentials{Token: "uninstalled-candidate", AccountID: "bot-1", UserID: "user-1"}, paths.credentials); err != nil {
		t.Fatalf("save crash-window credentials: %v", err)
	}
	for _, path := range []string{paths.context, recordedContextPath} {
		if err := store.NewContextStore("", path).Set("user-1", "stale-context"); err != nil {
			t.Fatalf("seed context %q: %v", path, err)
		}
	}
	marker := store.NewReauthStore(store.ReauthStatePath(paths.credentials))
	if err := marker.Mark(store.ReauthRecord{
		InvalidTokenSHA256: tokenSHA256("invalid-token"),
		AccountID:          "bot-1",
		ContextPaths:       []string{recordedContextPath},
	}); err != nil {
		t.Fatalf("seed reauthentication marker: %v", err)
	}

	bot := New(Options{
		AccountID:        "bot-1",
		CredPath:         paths.credentials,
		ContextTokenPath: paths.context,
		CursorPath:       paths.cursor,
		ReplayPath:       paths.replay,
		LogLevel:         "silent",
	})
	if !bot.ReauthRequired() {
		t.Fatal("durable marker was not visible before Login")
	}
	var loginRequests atomic.Int32
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		loginRequests.Add(1)
		return nil, fmt.Errorf("unexpected Login request: %s", req.URL.Path)
	})}
	if _, err := bot.Login(context.Background(), false); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("Login with durable marker = %v", err)
	}
	if !bot.ReauthRequired() {
		t.Fatal("durable marker did not publish terminal reauthentication state")
	}
	if got := loginRequests.Load(); got != 0 {
		t.Fatalf("Login with durable marker made %d network requests", got)
	}
	if _, err := os.Stat(paths.credentials); !os.IsNotExist(err) {
		t.Fatalf("candidate credentials survived marker recovery: %v", err)
	}
	for _, path := range []string{paths.context, recordedContextPath} {
		reloaded := store.NewContextStore("", path)
		if err := reloaded.Load(); err != nil {
			t.Fatalf("load cleared context %q: %v", path, err)
		}
		if len(reloaded.All()) != 0 {
			t.Fatalf("stale context survived at %q: %v", path, reloaded.All())
		}
	}

	markerDuringQR := false
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if _, err := os.Stat(marker.Path()); err != nil {
				return nil, fmt.Errorf("marker missing during QR request: %w", err)
			}
			markerDuringQR = true
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			body = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
		default:
			return nil, fmt.Errorf("unexpected reauthentication request: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	creds, err := bot.Reauthenticate(context.Background())
	if err != nil || creds.Token != "fresh-token" {
		t.Fatalf("Reauthenticate after marker recovery = %+v, %v", creds, err)
	}
	if !markerDuringQR {
		t.Fatal("fresh authentication did not retain marker across credential acquisition")
	}
	if _, err := os.Stat(marker.Path()); !os.IsNotExist(err) {
		t.Fatalf("marker survived successful installation: %v", err)
	}
}

func TestReauthenticationMarkerFailureDestroysStaleSessionState(t *testing.T) {
	dir := t.TempDir()
	blockedParent := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(dir, "credentials.json")
	contextPath := filepath.Join(dir, "context.json")
	bot := New(Options{
		AccountID:        "bot-1",
		CredPath:         credPath,
		ContextTokenPath: contextPath,
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay.json"),
		LogLevel:         "silent",
	})
	bot.reauthStore = store.NewReauthStore(filepath.Join(blockedParent, "marker.json"))
	bot.creds = &auth.Credentials{Token: "invalid-token", AccountID: "bot-1", BaseURL: "https://api.example"}
	if err := auth.SaveCredentials(bot.creds, credPath); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	if err := bot.contextTokens.Set("user-1", "stale-context"); err != nil {
		t.Fatalf("seed context: %v", err)
	}

	err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"})
	var reauthErr *ReauthRequiredError
	if !errors.As(err, &reauthErr) || reauthErr.CleanupErr == nil || !bot.ReauthRequired() {
		t.Fatalf("marker failure did not remain fail-closed: %v", err)
	}
	if got := bot.contextTokens.Get("user-1"); got != "" {
		t.Fatalf("best-effort context cleanup failed after marker error: %q", got)
	}
	if stored, loadErr := auth.LoadCredentials(credPath); loadErr != nil || stored != nil {
		t.Fatalf("stale credentials survived marker error: %+v, %v", stored, loadErr)
	}

	restarted := New(Options{
		AccountID:        "bot-1",
		CredPath:         credPath,
		ContextTokenPath: contextPath,
		CursorPath:       filepath.Join(dir, "restart-cursor.json"),
		ReplayPath:       filepath.Join(dir, "restart-replay.json"),
		LogLevel:         "silent",
	})
	restarted.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("fresh QR required")
	})}
	if creds, loginErr := restarted.Login(context.Background(), false); creds != nil || loginErr == nil {
		t.Fatalf("restart recovered invalid credentials: %+v, %v", creds, loginErr)
	}
}

func TestDurableReauthenticationRejectsSameTokenFingerprint(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	contextPath := filepath.Join(dir, "context.json")
	marker := store.NewReauthStore(store.ReauthStatePath(credPath))
	if err := marker.Mark(store.ReauthRecord{
		InvalidTokenSHA256: tokenSHA256("invalid-token"),
		AccountID:          "bot-1",
		ContextPaths:       []string{contextPath},
	}); err != nil {
		t.Fatal(err)
	}
	bot := New(Options{
		AccountID:        "bot-1",
		CredPath:         credPath,
		ContextTokenPath: contextPath,
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay.json"),
		LogLevel:         "silent",
	})
	bot.client.HTTP = reauthenticationTestClient(rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
	}), "invalid-token", "bot-1", "https://fresh.example")

	creds, err := bot.Reauthenticate(context.Background())
	if creds != nil || !errors.Is(err, auth.ErrInvalidatedCredential) || !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("same-token durable reauthentication = %+v, %v", creds, err)
	}
	if !bot.ReauthRequired() {
		t.Fatal("same-token response cleared the durable marker")
	}
	if stored, loadErr := auth.LoadCredentials(credPath); loadErr != nil || stored != nil {
		t.Fatalf("same-token candidate remained persisted: %+v, %v", stored, loadErr)
	}
}

func TestReauthenticationMarkerRemovalFailureDoesNotPublishSession(t *testing.T) {
	bot, paths := newAuthenticatedSessionTestBot(t, "https://old.example")
	if err := bot.handleAuthenticatedError(&APIError{RetCode: -14, Message: "expired"}); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalidate: %v", err)
	}
	bot.reauthStore = &clearFailingReauthPersistence{reauthPersistence: bot.reauthStore}
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		if req.URL.Path == "/ilink/bot/get_qrcode_status" {
			body = `{"status":"confirmed","bot_token":"fresh-token","ilink_bot_id":"bot-1","ilink_user_id":"user-1","baseurl":"https://fresh.example"}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	creds, err := bot.Reauthenticate(context.Background())
	if creds != nil || !errors.Is(err, ErrReauthRequired) || !bot.ReauthRequired() {
		t.Fatalf("marker removal failure published session: %+v, %v", creds, err)
	}
	if stored, loadErr := auth.LoadCredentials(paths.credentials); loadErr != nil || stored != nil {
		t.Fatalf("fresh credentials survived failed marker removal: %+v, %v", stored, loadErr)
	}
}

func TestDurableReauthenticationRejectsConfiguredAccountMismatch(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	marker := store.NewReauthStore(store.ReauthStatePath(credPath))
	if err := marker.Mark(store.ReauthRecord{
		InvalidTokenSHA256: tokenSHA256("invalid-token"),
		AccountID:          "bot-a",
		ContextPaths:       []string{filepath.Join(dir, "bot-a-context.json")},
	}); err != nil {
		t.Fatal(err)
	}
	bot := New(Options{
		AccountID:        "bot-b",
		CredPath:         credPath,
		ContextTokenPath: filepath.Join(dir, "bot-b-context.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay.json"),
		LogLevel:         "silent",
	})
	var requests atomic.Int32
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected network request")
	})}

	if creds, err := bot.Reauthenticate(context.Background()); creds != nil || !errors.Is(err, ErrReauthRequired) || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("configured account mismatch = %+v, %v", creds, err)
	}
	if requests.Load() != 0 || !bot.ReauthRequired() {
		t.Fatalf("configured mismatch requests=%d required=%v", requests.Load(), bot.ReauthRequired())
	}
	record, err := marker.Load()
	if err != nil || record == nil || record.AccountID != "bot-a" {
		t.Fatalf("configured mismatch rewrote marker: %+v, %v", record, err)
	}
}

type clearFailingReauthPersistence struct {
	reauthPersistence
}

func (*clearFailingReauthPersistence) Clear() error {
	return errors.New("injected marker removal failure")
}

func TestLoginRejectsCollidingStatePathsBeforeNetwork(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	tests := []struct {
		name        string
		contextPath string
	}{
		{name: "credentials", contextPath: filepath.Join(dir, "nested", "..", "credentials.json")},
		{name: "marker", contextPath: store.ReauthStatePath(credPath)},
	}
	if runtime.GOOS == "windows" {
		tests = append(tests, struct {
			name        string
			contextPath string
		}{name: "windows-case", contextPath: strings.ToUpper(credPath)})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bot := New(Options{
				AccountID:        "bot-1",
				CredPath:         credPath,
				ContextTokenPath: tc.contextPath,
				CursorPath:       filepath.Join(dir, tc.name+"-cursor.json"),
				ReplayPath:       filepath.Join(dir, tc.name+"-replay.json"),
				LogLevel:         "silent",
			})
			var requests atomic.Int32
			bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected network request")
			})}
			if creds, err := bot.Login(context.Background(), false); creds != nil || err == nil || !strings.Contains(err.Error(), "state paths must be distinct") {
				t.Fatalf("colliding path Login = %+v, %v", creds, err)
			}
			if requests.Load() != 0 {
				t.Fatalf("colliding path made %d requests", requests.Load())
			}
		})
	}
}

func TestMalformedReauthenticationMarkerCanBeRepairedExplicitly(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "credentials.json")
	contextPath := filepath.Join(dir, "context.json")
	markerPath := store.ReauthStatePath(credPath)
	if err := auth.SaveCredentials(&auth.Credentials{Token: "invalid-token", AccountID: "bot-1", UserID: "user-1"}, credPath); err != nil {
		t.Fatal(err)
	}
	if err := store.NewContextStore("", contextPath).Set("user-1", "stale-context"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	bot := New(Options{AccountID: "bot-1", CredPath: credPath, ContextTokenPath: contextPath, LogLevel: "silent"})
	var requests atomic.Int32
	bot.client.HTTP = &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, fmt.Errorf("unexpected Login request: %s", req.URL.Path)
	})}
	if _, err := bot.Login(context.Background(), false); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("Login with malformed marker = %v", err)
	}
	if !bot.ReauthRequired() || requests.Load() != 0 {
		t.Fatalf("malformed marker state=%v requests=%d", bot.ReauthRequired(), requests.Load())
	}
	if record, err := store.NewReauthStore(markerPath).Load(); err != nil || record == nil || record.InvalidTokenSHA256 != tokenSHA256("invalid-token") {
		t.Fatalf("repaired marker = %+v, %v", record, err)
	}

	bot.client.HTTP = reauthenticationTestClient(rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unexpected request: %s", req.URL.Path)
	}), "fresh-token", "bot-1", "https://fresh.example")
	if creds, err := bot.Reauthenticate(context.Background()); err != nil || creds.Token != "fresh-token" {
		t.Fatalf("explicit repair Reauthenticate = %+v, %v", creds, err)
	}
}

type rootRoundTripFunc func(*http.Request) (*http.Response, error)

func (f rootRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func reauthenticationTestClient(base http.RoundTripper, token, accountID, baseURL string) *http.Client {
	return &http.Client{Transport: rootRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			body = `{"qrcode":"qr","qrcode_img_content":"https://example.com/qr"}`
		case "/ilink/bot/get_qrcode_status":
			body = fmt.Sprintf(`{"status":"confirmed","bot_token":%q,"ilink_bot_id":%q,"ilink_user_id":"user-1","baseurl":%q}`, token, accountID, baseURL)
		default:
			return base.RoundTrip(req)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
}

type blockingRandomReader struct {
	delegate io.Reader
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (r *blockingRandomReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.delegate.Read(p)
}

type blockingNthRandomReader struct {
	delegate io.Reader
	started  chan struct{}
	release  chan struct{}
	blockAt  int32
	reads    atomic.Int32
	once     sync.Once
}

func (r *blockingNthRandomReader) Read(p []byte) (int, error) {
	if r.reads.Add(1) == r.blockAt {
		r.once.Do(func() { close(r.started) })
		<-r.release
	}
	return r.delegate.Read(p)
}

type testReauthenticationResult struct {
	creds *Credentials
	err   error
}

func startTestReauthentication(bot *Bot) <-chan testReauthenticationResult {
	done := make(chan testReauthenticationResult, 1)
	go func() {
		creds, err := bot.Reauthenticate(context.Background())
		done <- testReauthenticationResult{creds: creds, err: err}
	}()
	return done
}

func waitForReauthenticationWriter(t *testing.T, bot *Bot, done <-chan testReauthenticationResult, blockedBy string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case result := <-done:
			t.Fatalf("Reauthenticate completed while %s remained blocked: credentials=%+v error=%v", blockedBy, result.creds, result.err)
		default:
		}
		if !bot.requestMu.TryRLock() {
			return
		}
		bot.requestMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Reauthenticate writer behind %s", blockedBy)
		}
		runtime.Gosched()
	}
}

func waitForTestReauthentication(t *testing.T, done <-chan testReauthenticationResult) testReauthenticationResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Reauthenticate")
		return testReauthenticationResult{}
	}
}

func waitForTestRequest(t *testing.T, done <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for old %s request", label)
		return nil
	}
}

func isExpectedOldRequestResult(err error) bool {
	return err == nil || errors.Is(err, ErrSessionChanged) || errors.Is(err, ErrReauthRequired)
}

func reauthReplayKey(messageID int64, userID string) string {
	return replayKeys(&WireMessage{
		MessageID:   messageID,
		FromUserID:  userID,
		MessageType: MessageTypeUser,
	})[0]
}

func newAuthenticatedSessionTestBot(t *testing.T, baseURL string) (*Bot, sessionTestPaths) {
	t.Helper()
	dir := t.TempDir()
	paths := sessionTestPaths{
		credentials: filepath.Join(dir, "credentials.json"),
		context:     filepath.Join(dir, "context.json"),
		cursor:      filepath.Join(dir, "cursor.json"),
		replay:      filepath.Join(dir, "replay.json"),
	}
	bot := New(Options{
		BaseURL:          baseURL,
		AccountID:        "bot-1",
		CredPath:         paths.credentials,
		ContextTokenPath: paths.context,
		CursorPath:       paths.cursor,
		ReplayPath:       paths.replay,
		LogLevel:         "silent",
	})
	creds := &auth.Credentials{
		Token:     "invalid-token",
		BaseURL:   baseURL,
		AccountID: "bot-1",
		UserID:    "user-1",
	}
	if err := auth.SaveCredentials(creds, paths.credentials); err != nil {
		t.Fatalf("save credentials: %v", err)
	}
	if err := bot.contextTokens.Set("user-1", "context-1"); err != nil {
		t.Fatalf("set context token: %v", err)
	}
	sessionContext, cancelSession := context.WithCancel(context.Background())
	t.Cleanup(cancelSession)
	bot.mu.Lock()
	bot.creds = creds
	bot.configCache = bot.newConfigCache(creds, bot.sessionGeneration)
	bot.sessionContext = sessionContext
	bot.cancelSession = cancelSession
	bot.mu.Unlock()
	return bot, paths
}
