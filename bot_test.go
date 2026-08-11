package wechatbot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/crypto"
)

func TestChunkTextShort(t *testing.T) {
	chunks := chunkText("hello", 100)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Fatalf("expected single chunk, got %v", chunks)
	}
}

func TestChunkTextEmpty(t *testing.T) {
	chunks := chunkText("", 100)
	if len(chunks) != 1 || chunks[0] != "" {
		t.Fatalf("expected single empty chunk, got %v", chunks)
	}
}

func TestChunkTextSplitsOnParagraph(t *testing.T) {
	text := "aaaa\n\nbbbb"
	chunks := chunkText(text, 7)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "aaaa\n\n" || chunks[1] != "bbbb" {
		t.Fatalf("unexpected chunks: %v", chunks)
	}
}

func TestChunkTextSplitsOnNewline(t *testing.T) {
	text := "aaaa\nbbbb"
	chunks := chunkText(text, 7)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "aaaa\n" || chunks[1] != "bbbb" {
		t.Fatalf("unexpected chunks: %v", chunks)
	}
}

func TestChunkTextPreservesUTF8(t *testing.T) {
	text := strings.Repeat("你", maxTextChars+1)
	chunks := chunkText(text, maxTextChars)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk is not valid UTF-8: %q", chunk)
		}
		if runeLen(chunk) > maxTextChars {
			t.Fatalf("chunk has %d runes, want <= %d", runeLen(chunk), maxTextChars)
		}
	}
}

func TestDetectTypeText(t *testing.T) {
	items := []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hi"}}}
	if detectType(items) != ContentText {
		t.Fatal("expected text")
	}
}

func TestDetectTypeImage(t *testing.T) {
	items := []MessageItem{{Type: ItemImage, ImageItem: &ImageItem{URL: "http://img"}}}
	if detectType(items) != ContentImage {
		t.Fatal("expected image")
	}
}

func TestDetectTypeVoice(t *testing.T) {
	items := []MessageItem{{Type: ItemVoice, VoiceItem: &VoiceItem{Text: "hello"}}}
	if detectType(items) != ContentVoice {
		t.Fatal("expected voice")
	}
}

func TestDetectTypeFile(t *testing.T) {
	items := []MessageItem{{Type: ItemFile, FileItem: &FileItem{FileName: "doc.pdf"}}}
	if detectType(items) != ContentFile {
		t.Fatal("expected file")
	}
}

func TestDetectTypeVideo(t *testing.T) {
	items := []MessageItem{{Type: ItemVideo, VideoItem: &VideoItem{}}}
	if detectType(items) != ContentVideo {
		t.Fatal("expected video")
	}
}

func TestDetectTypeEmpty(t *testing.T) {
	if detectType(nil) != ContentText {
		t.Fatal("expected text for empty items")
	}
}

func TestExtractTextSingle(t *testing.T) {
	items := []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "hello world"}}}
	if extractText(items) != "hello world" {
		t.Fatal("unexpected text")
	}
}

func TestExtractTextMulti(t *testing.T) {
	items := []MessageItem{
		{Type: ItemText, TextItem: &TextItem{Text: "line1"}},
		{Type: ItemText, TextItem: &TextItem{Text: "line2"}},
	}
	if extractText(items) != "line1\nline2" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestExtractTextImageURL(t *testing.T) {
	items := []MessageItem{{Type: ItemImage, ImageItem: &ImageItem{URL: "http://img.jpg"}}}
	if extractText(items) != "http://img.jpg" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestExtractTextImagePlaceholder(t *testing.T) {
	items := []MessageItem{{Type: ItemImage, ImageItem: &ImageItem{}}}
	if extractText(items) != "[image]" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestExtractTextVoiceWithText(t *testing.T) {
	items := []MessageItem{{Type: ItemVoice, VoiceItem: &VoiceItem{Text: "hello"}}}
	if extractText(items) != "hello" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestExtractTextVoicePlaceholder(t *testing.T) {
	items := []MessageItem{{Type: ItemVoice, VoiceItem: &VoiceItem{}}}
	if extractText(items) != "[voice]" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestExtractTextFile(t *testing.T) {
	items := []MessageItem{{Type: ItemFile, FileItem: &FileItem{FileName: "report.pdf"}}}
	if extractText(items) != "report.pdf" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestExtractTextVideo(t *testing.T) {
	items := []MessageItem{{Type: ItemVideo, VideoItem: &VideoItem{}}}
	if extractText(items) != "[video]" {
		t.Fatalf("unexpected text: %q", extractText(items))
	}
}

func TestParseMessageUserText(t *testing.T) {
	b := New()
	wire := &WireMessage{
		FromUserID:   "user123",
		ToUserID:     "bot456",
		ClientID:     "c1",
		CreateTimeMs: 1700000000000,
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "ctx-abc",
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		},
	}
	msg := b.parseMessage(wire)
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if msg.UserID != "user123" {
		t.Fatalf("unexpected user ID: %s", msg.UserID)
	}
	if msg.Text != "hello" {
		t.Fatalf("unexpected text: %s", msg.Text)
	}
	if msg.Type != ContentText {
		t.Fatalf("unexpected type: %s", msg.Type)
	}
	if msg.ContextToken != "ctx-abc" {
		t.Fatalf("unexpected context token: %s", msg.ContextToken)
	}
	expectedTime := time.UnixMilli(1700000000000)
	if !msg.Timestamp.Equal(expectedTime) {
		t.Fatalf("unexpected timestamp: %v", msg.Timestamp)
	}
}

func TestParseMessageSkipsBot(t *testing.T) {
	b := New()
	wire := &WireMessage{
		FromUserID:   "bot456",
		ToUserID:     "user123",
		MessageType:  MessageTypeBot,
		MessageState: MessageStateFinish,
		ContextToken: "ctx-abc",
		ItemList:     []MessageItem{{Type: ItemText, TextItem: &TextItem{Text: "reply"}}},
	}
	msg := b.parseMessage(wire)
	if msg != nil {
		t.Fatal("expected nil for bot message")
	}
}

func TestParseMessageWithImage(t *testing.T) {
	b := New()
	wire := &WireMessage{
		FromUserID:   "user123",
		ToUserID:     "bot456",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "ctx-abc",
		ItemList: []MessageItem{
			{Type: ItemImage, ImageItem: &ImageItem{
				URL:         "http://img.jpg",
				ThumbWidth:  100,
				ThumbHeight: 200,
			}},
		},
	}
	msg := b.parseMessage(wire)
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if len(msg.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(msg.Images))
	}
	if msg.Images[0].URL != "http://img.jpg" {
		t.Fatalf("unexpected URL: %s", msg.Images[0].URL)
	}
	if msg.Images[0].Width != 100 || msg.Images[0].Height != 200 {
		t.Fatalf("unexpected dimensions: %dx%d", msg.Images[0].Width, msg.Images[0].Height)
	}
}

func TestParseMessageWithQuoted(t *testing.T) {
	b := New()
	wire := &WireMessage{
		FromUserID:   "user123",
		ToUserID:     "bot456",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "ctx-abc",
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "replying"},
				RefMsg: &RefMessage{
					Title:       "Original",
					MessageItem: &MessageItem{Type: ItemText, TextItem: &TextItem{Text: "original text"}},
				}},
		},
	}
	msg := b.parseMessage(wire)
	if msg == nil {
		t.Fatal("expected non-nil message")
	}
	if msg.QuotedMessage == nil {
		t.Fatal("expected quoted message")
	}
	if msg.QuotedMessage.Title != "Original" {
		t.Fatalf("unexpected title: %s", msg.QuotedMessage.Title)
	}
	if msg.QuotedMessage.Text != "original text" {
		t.Fatalf("unexpected quoted text: %s", msg.QuotedMessage.Text)
	}
}

func TestRememberContextUser(t *testing.T) {
	b := New(Options{ContextTokenPath: filepath.Join(t.TempDir(), "context_tokens.json")})
	wire := &WireMessage{
		FromUserID:   "user123",
		ToUserID:     "bot456",
		MessageType:  MessageTypeUser,
		ContextToken: "ctx-new",
	}
	if err := b.rememberContext(wire); err != nil {
		t.Fatalf("remember context: %v", err)
	}
	if ct := b.contextTokens.Get("user123"); ct != "ctx-new" {
		t.Fatalf("expected context token ctx-new, got %v", ct)
	}
}

func TestRememberContextBot(t *testing.T) {
	b := New(Options{ContextTokenPath: filepath.Join(t.TempDir(), "context_tokens.json")})
	wire := &WireMessage{
		FromUserID:   "bot456",
		ToUserID:     "user123",
		MessageType:  MessageTypeBot,
		ContextToken: "ctx-bot",
	}
	if err := b.rememberContext(wire); err != nil {
		t.Fatalf("remember context: %v", err)
	}
	if ct := b.contextTokens.Get("user123"); ct != "ctx-bot" {
		t.Fatalf("expected context token ctx-bot for toUserID, got %v", ct)
	}
}

func TestReplyWithoutLoginReturnsError(t *testing.T) {
	dir := t.TempDir()
	b := New(Options{ContextTokenPath: filepath.Join(dir, "context_tokens.json")})
	err := b.Reply(context.Background(), &IncomingMessage{
		UserID:       "user123",
		ContextToken: "ctx",
	}, "hello")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not logged in error, got %v", err)
	}
}

func TestSendTypingWithoutLoginReturnsError(t *testing.T) {
	dir := t.TempDir()
	b := New(Options{ContextTokenPath: filepath.Join(dir, "context_tokens.json")})
	if err := b.contextTokens.Set("user123", "ctx"); err != nil {
		t.Fatal(err)
	}
	err := b.SendTyping(context.Background(), "user123")
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not logged in error, got %v", err)
	}
}

func TestReplyContentWithoutLoginDoesNotDownloadRemote(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte("image"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	b := New(Options{ContextTokenPath: filepath.Join(dir, "context_tokens.json")})
	err := b.ReplyContent(context.Background(), &IncomingMessage{
		UserID:       "user123",
		ContextToken: "ctx",
	}, SendImageURL(ts.URL))
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not logged in error, got %v", err)
	}
	if called {
		t.Fatal("remote URL should not be fetched before login check")
	}
}

func TestSendBlockedWhenSessionPaused(t *testing.T) {
	dir := t.TempDir()
	b := New(Options{ContextTokenPath: filepath.Join(dir, "context_tokens.json")})
	if err := b.contextTokens.Set("user123", "ctx"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	b.mu.Unlock()
	b.sessionGuard.Pause()

	err := b.Send(context.Background(), "user123", "hello")
	if err == nil || !strings.Contains(err.Error(), "session paused") {
		t.Fatalf("expected session paused error, got %v", err)
	}
}

func TestDownloadRawUsesFullURL(t *testing.T) {
	key := []byte("1234567890abcdef")
	ciphertext, err := crypto.EncryptAESECB([]byte("hello"), key)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(ciphertext)
	}))
	defer ts.Close()

	b := New()
	data, err := b.DownloadRaw(context.Background(), &CDNMedia{
		FullURL: ts.URL,
		AESKey:  crypto.EncodeAESKeyBase64(key),
	}, "")
	if err != nil {
		t.Fatalf("DownloadRaw failed: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected hello, got %q", data)
	}
}

func TestWireMessageJSON(t *testing.T) {
	wire := WireMessage{
		FromUserID:   "user1",
		ToUserID:     "bot1",
		ClientID:     "c1",
		CreateTimeMs: 1700000000000,
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "ctx",
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		},
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded WireMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.FromUserID != wire.FromUserID || decoded.MessageType != wire.MessageType {
		t.Fatalf("round-trip mismatch: %+v", decoded)
	}
	if len(decoded.ItemList) != 1 || decoded.ItemList[0].TextItem.Text != "hello" {
		t.Fatal("item list mismatch")
	}
}

func TestTypesEnumValues(t *testing.T) {
	if MessageTypeUser != 1 || MessageTypeBot != 2 {
		t.Fatal("MessageType enum mismatch")
	}
	if MessageStateNew != 0 || MessageStateGenerating != 1 || MessageStateFinish != 2 {
		t.Fatal("MessageState enum mismatch")
	}
	if ItemText != 1 || ItemImage != 2 || ItemVoice != 3 || ItemFile != 4 || ItemVideo != 5 {
		t.Fatal("MessageItemType enum mismatch")
	}
}

func TestCategorizeByExtension(t *testing.T) {
	tests := []struct{ name, want string }{
		{"photo.png", "image"},
		{"photo.JPG", "image"},
		{"anim.gif", "image"},
		{"clip.mp4", "video"},
		{"clip.MOV", "video"},
		{"report.pdf", "file"},
		{"data.csv", "file"},
		{"noext", "file"},
	}
	for _, tc := range tests {
		got := categorizeByExtension(tc.name)
		if got != tc.want {
			t.Errorf("categorizeByExtension(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCdnMediaMap(t *testing.T) {
	m := &CDNMedia{EncryptQueryParam: "param=1", AESKey: "key123", EncryptType: 1}
	d := cdnMediaMap(m)
	if d["encrypt_query_param"] != "param=1" || d["aes_key"] != "key123" || d["encrypt_type"] != 1 {
		t.Fatalf("unexpected cdnMediaMap result: %v", d)
	}
}

func TestSendContentConstructors(t *testing.T) {
	s := SendText("hello")
	if s.Text != "hello" {
		t.Fatalf("SendText: got %q", s.Text)
	}
	s = SendImage([]byte{1, 2, 3})
	if len(s.Image) != 3 {
		t.Fatalf("SendImage: got len %d", len(s.Image))
	}
	s = SendVideo([]byte{4, 5})
	if len(s.Video) != 2 {
		t.Fatalf("SendVideo: got len %d", len(s.Video))
	}
	s = SendFile([]byte{6}, "test.pdf")
	if len(s.File) != 1 || s.FileName != "test.pdf" {
		t.Fatalf("SendFile: got len=%d name=%q", len(s.File), s.FileName)
	}
}

func TestCredentialsJSON(t *testing.T) {
	creds := Credentials{
		Token:     "tok",
		BaseURL:   "https://api.example.com",
		AccountID: "acc1",
		UserID:    "uid1",
		SavedAt:   "2024-01-01T00:00:00Z",
	}
	data, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Credentials
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(creds, decoded) {
		t.Fatalf("round-trip mismatch: %+v vs %+v", creds, decoded)
	}
	// Verify JSON field names
	var m map[string]interface{}
	json.Unmarshal(data, &m)
	if _, ok := m["baseUrl"]; !ok {
		t.Fatal("expected camelCase 'baseUrl' in JSON")
	}
	if _, ok := m["accountId"]; !ok {
		t.Fatal("expected camelCase 'accountId' in JSON")
	}
}

func TestReplayKeys(t *testing.T) {
	tests := []struct {
		name string
		wire *WireMessage
		want []string
	}{
		{
			name: "all user message aliases",
			wire: &WireMessage{
				MessageID:   42,
				ClientID:    "client-1",
				Seq:         7,
				FromUserID:  "user-1",
				ToUserID:    "bot-1",
				MessageType: MessageTypeUser,
			},
			want: []string{
				"peer:6:user-1:message:42",
				"peer:6:user-1:client:client-1",
				"peer:6:user-1:seq:7",
			},
		},
		{
			name: "bot message uses recipient as peer",
			wire: &WireMessage{
				MessageID:   42,
				FromUserID:  "bot-1",
				ToUserID:    "user-1",
				MessageType: MessageTypeBot,
			},
			want: []string{"peer:6:user-1:message:42"},
		},
		{
			name: "missing identity",
			wire: &WireMessage{FromUserID: "user-1", MessageType: MessageTypeUser},
		},
		{
			name: "missing peer",
			wire: &WireMessage{MessageID: 42, MessageType: MessageTypeUser},
		},
		{name: "nil message"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replayKeys(tc.wire); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("replayKeys() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProcessUpdateBatchRejectsMalformedMessageWithoutAdvancingCursor(t *testing.T) {
	bot := newReplayTestBot(t)
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult { return AckMessage() })
	if err := bot.cursorStore.Set("cursor-before"); err != nil {
		t.Fatal(err)
	}

	err := bot.processUpdateBatch(context.Background(), handler, updateBatch{
		messages: []json.RawMessage{json.RawMessage(`{"message_id":"invalid"}`)},
		cursor:   "cursor-after",
	})
	if err == nil {
		t.Fatal("expected malformed message error")
	}
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("error does not preserve JSON decode cause: %v", err)
	}
	if got := bot.cursorStore.Get(); got != "cursor-before" {
		t.Fatalf("cursor advanced after malformed message: %q", got)
	}
}

func TestRunRetriesMalformedMessageFromPersistedCursor(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		ContextTokenPath: filepath.Join(dir, "context_tokens.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay_dedupe.json"),
	}
	seed := New(opts)
	if err := seed.cursorStore.Set("cursor-before"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		firstCursor := make(chan string, 1)
		releasePoll := make(chan struct{})
		var polls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/ilink/bot/msg/notifystart", "/ilink/bot/msg/notifystop":
				_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
			case "/ilink/bot/getupdates":
				if polls.Add(1) != 1 {
					select {
					case <-releasePoll:
					case <-r.Context().Done():
					}
					return
				}
				var request struct {
					Cursor string `json:"get_updates_buf"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode getupdates request: %v", err)
					return
				}
				firstCursor <- request.Cursor
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ret":             0,
					"msgs":            []json.RawMessage{json.RawMessage(`{"message_id":"invalid"}`)},
					"get_updates_buf": "cursor-after",
				})
			default:
				http.NotFound(w, r)
			}
		}))

		bot := New(opts)
		bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult { return AckMessage() }))
		bot.client.HTTP = server.Client()
		bot.creds = &auth.Credentials{BaseURL: server.URL, Token: "token", AccountID: "bot-1"}
		err := bot.Run(context.Background())
		close(releasePoll)
		server.Close()
		if err == nil {
			t.Fatalf("attempt %d: expected malformed message error", attempt)
		}
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("attempt %d: error does not preserve JSON decode cause: %v", attempt, err)
		}
		select {
		case got := <-firstCursor:
			if got != "cursor-before" {
				t.Fatalf("attempt %d: poll cursor = %q, want cursor-before", attempt, got)
			}
		default:
			t.Fatalf("attempt %d: getupdates request was not observed", attempt)
		}

		persisted := New(opts)
		if err := persisted.cursorStore.Load(); err != nil {
			t.Fatalf("attempt %d: reload cursor: %v", attempt, err)
		}
		if got := persisted.cursorStore.Get(); got != "cursor-before" {
			t.Fatalf("attempt %d: persisted cursor = %q, want cursor-before", attempt, got)
		}
	}
}

func TestProcessUpdateBatchScopesIdentityByPeer(t *testing.T) {
	bot := newReplayTestBot(t)

	var handled atomic.Int32
	handler := MessageHandlerFunc(func(ctx context.Context, msg *IncomingMessage) MessageResult {
		handled.Add(1)
		return AckMessage()
	})

	if err := bot.processUpdateBatch(context.Background(), handler, updateBatch{
		messages: []json.RawMessage{
			marshalWireMessage(t, WireMessage{
				MessageID:   42,
				FromUserID:  "user-1",
				ToUserID:    "bot-1",
				MessageType: MessageTypeUser,
			}),
			marshalWireMessage(t, WireMessage{
				MessageID:   42,
				FromUserID:  "user-2",
				ToUserID:    "bot-1",
				MessageType: MessageTypeUser,
			}),
		},
		cursor: "cursor-1",
	}); err != nil {
		t.Fatalf("process batch: %v", err)
	}
	if got := handled.Load(); got != 2 {
		t.Fatalf("handler called %d times, want 2", got)
	}
	for _, key := range []string{
		"peer:6:user-1:message:42",
		"peer:6:user-2:message:42",
	} {
		if !bot.replayStore.SeenAny(key) {
			t.Fatalf("handled identity %q was not persisted", key)
		}
	}
}

func TestProcessUpdateBatchDeduplicatesAnyCommittedAlias(t *testing.T) {
	bot := newReplayTestBot(t)

	var handled atomic.Int32
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		handled.Add(1)
		return AckMessage()
	})
	messages := []WireMessage{
		{
			MessageID:   42,
			ClientID:    "client-1",
			Seq:         7,
			FromUserID:  "user-1",
			ToUserID:    "bot-1",
			MessageType: MessageTypeUser,
		},
		{
			ClientID:    "client-1",
			FromUserID:  "user-1",
			ToUserID:    "bot-1",
			MessageType: MessageTypeUser,
		},
		{
			Seq:         7,
			FromUserID:  "user-1",
			ToUserID:    "bot-1",
			MessageType: MessageTypeUser,
		},
	}
	for i, wire := range messages {
		if err := bot.processUpdateBatch(context.Background(), handler, updateBatch{
			messages: []json.RawMessage{marshalWireMessage(t, wire)},
			cursor:   "cursor-" + strconv.Itoa(i+1),
		}); err != nil {
			t.Fatalf("delivery %d failed: %v", i+1, err)
		}
	}

	if got := handled.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1", got)
	}
	for _, key := range replayKeys(&messages[0]) {
		if !bot.replayStore.SeenAny(key) {
			t.Fatalf("handled identity %q was not persisted", key)
		}
	}
	if got := bot.cursorStore.Get(); got != "cursor-3" {
		t.Fatalf("cursor = %q, want cursor-3", got)
	}
}

func TestProcessUpdateBatchPrefersMessageIDOverCollidingFallbackAliases(t *testing.T) {
	bot := newReplayTestBot(t)

	var handled atomic.Int32
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		handled.Add(1)
		return AckMessage()
	})
	messages := []WireMessage{
		{
			MessageID:   1,
			ClientID:    "client-1",
			Seq:         7,
			FromUserID:  "user-1",
			MessageType: MessageTypeUser,
		},
		{
			MessageID:   2,
			ClientID:    "client-1",
			Seq:         7,
			FromUserID:  "user-1",
			MessageType: MessageTypeUser,
		},
		{
			ClientID:    "client-1",
			FromUserID:  "user-1",
			MessageType: MessageTypeUser,
		},
		{
			Seq:         7,
			FromUserID:  "user-1",
			MessageType: MessageTypeUser,
		},
	}
	for i, wire := range messages {
		if err := bot.processUpdateBatch(context.Background(), handler, updateBatch{
			messages: []json.RawMessage{marshalWireMessage(t, wire)},
		}); err != nil {
			t.Fatalf("delivery %d failed: %v", i+1, err)
		}
	}

	if got := handled.Load(); got != 2 {
		t.Fatalf("handler called %d times, want 2", got)
	}
	for _, key := range []string{
		"peer:6:user-1:message:1",
		"peer:6:user-1:message:2",
		"peer:6:user-1:client:client-1",
		"peer:6:user-1:seq:7",
	} {
		if !bot.replayStore.SeenAny(key) {
			t.Fatalf("handled identity %q was not persisted", key)
		}
	}
}

func TestProcessUpdateBatchKeepsAtLeastOnceWithoutIdentityOrPeer(t *testing.T) {
	bot := newReplayTestBot(t)

	var handled atomic.Int32
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		handled.Add(1)
		return AckMessage()
	})
	messages := []WireMessage{
		{FromUserID: "user-1", ToUserID: "bot-1", MessageType: MessageTypeUser},
		{MessageID: 42, ToUserID: "bot-1", MessageType: MessageTypeUser},
	}
	for _, wire := range messages {
		if keys := replayKeys(&wire); len(keys) != 0 {
			t.Fatalf("replayKeys(%+v) = %q, want no keys", wire, keys)
		}
		for delivery := 0; delivery < 2; delivery++ {
			if err := bot.processUpdateBatch(context.Background(), handler, updateBatch{
				messages: []json.RawMessage{marshalWireMessage(t, wire)},
			}); err != nil {
				t.Fatalf("process message: %v", err)
			}
		}
	}

	if got := handled.Load(); got != 4 {
		t.Fatalf("handler called %d times, want 4", got)
	}
}

func TestRunContinuesPollingBeforeHandlerCompletes(t *testing.T) {
	dir := t.TempDir()
	raw := marshalWireMessage(t, WireMessage{
		MessageID:    101,
		FromUserID:   "user-1",
		ToUserID:     "bot-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "context-1",
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		},
	})

	var polls atomic.Int32
	secondPollStarted := make(chan struct{}, 1)
	releaseSecondPoll := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/msg/notifystart", "/ilink/bot/msg/notifystop":
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
			case secondPollStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseSecondPoll:
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer func() {
		select {
		case releaseSecondPoll <- struct{}{}:
		default:
		}
	}()

	bot := New(Options{
		ContextTokenPath: filepath.Join(dir, "context_tokens.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay_dedupe.json"),
	})
	bot.client.HTTP = server.Client()
	bot.creds = &auth.Credentials{BaseURL: server.URL, Token: "token", AccountID: "bot-1"}

	handlerStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	bot.Handle(MessageHandlerFunc(func(ctx context.Context, msg *IncomingMessage) MessageResult {
		select {
		case handlerStarted <- struct{}{}:
		default:
		}
		<-releaseHandler
		return AckMessage()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- bot.Run(ctx)
	}()

	waitForSignal(t, handlerStarted, "handler start")
	waitForSignal(t, secondPollStarted, "second getUpdates request")
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor committed before handler completion: %q", got)
	}

	cancel()
	releaseSecondPoll <- struct{}{}
	close(releaseHandler)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if got := bot.cursorStore.Get(); got != "cursor-1" {
		t.Fatalf("cursor = %q after handler completion, want cursor-1", got)
	}
}

func TestReplyRejectsMissingContextTokenBeforeSend(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
	}))
	defer server.Close()

	bot := New()
	bot.client.HTTP = server.Client()
	bot.creds = &auth.Credentials{BaseURL: server.URL, Token: "token"}
	err := bot.Reply(context.Background(), &IncomingMessage{UserID: "user-1"}, "hello")
	if err == nil || !strings.Contains(err.Error(), "no context_token") {
		t.Fatalf("expected missing context_token error, got %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("unexpected network requests: %d", got)
	}
}

func TestReplyContentRejectsMissingContextTokenBeforeDownload(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write([]byte("image"))
	}))
	defer server.Close()

	bot := New()
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	err := bot.ReplyContent(
		context.Background(),
		&IncomingMessage{UserID: "user-1"},
		SendImageURL(server.URL),
	)
	if err == nil || !strings.Contains(err.Error(), "no context_token") {
		t.Fatalf("expected missing context_token error, got %v", err)
	}
	if got := downloads.Load(); got != 0 {
		t.Fatalf("remote media downloaded before token validation: %d", got)
	}
}

func newReplayTestBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	bot := New(Options{
		ContextTokenPath: filepath.Join(dir, "context_tokens.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay_dedupe.json"),
	})
	if err := bot.replayStore.Load(); err != nil {
		t.Fatal(err)
	}
	return bot
}

func marshalWireMessage(t *testing.T, wire WireMessage) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func waitForSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}
