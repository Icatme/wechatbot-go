package wechatbot

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/config"
	"github.com/Icatme/wechatbot-go/internal/crypto"
	"github.com/Icatme/wechatbot-go/internal/markdown"
	"github.com/Icatme/wechatbot-go/internal/protocol"
	"github.com/Icatme/wechatbot-go/internal/remote"
	"github.com/Icatme/wechatbot-go/internal/session"
	"github.com/Icatme/wechatbot-go/internal/store"
)

// MessageHandler is called for each incoming user message.
type MessageHandler func(msg *IncomingMessage)

// Options configures a Bot instance.
type Options struct {
	BaseURL          string
	AccountID        string // optional account identifier for multi-bot isolation
	CredPath         string
	ContextTokenPath string
	CursorPath       string
	BotAgent         string // UA-style, e.g. "MyBot/1.2.0"
	RouteTag         string // sent as SKRouteTag header
	StripMarkdown    bool   // strip markdown from outbound text
	NotifyErrors     bool   // automatically notify user on send failure
	LogLevel         string // "debug", "info", "warn", "error", "silent"
	OnQRURL          func(url string)
	OnScanned        func()
	OnExpired        func()
	OnVerifyCode     func() (string, error)
	OnError          func(err error)
}

// Bot is the main WeChat bot client.
type Bot struct {
	opts          Options
	client        *protocol.Client
	creds         *auth.Credentials
	configCache   *config.Cache
	sessionGuard  *session.Guard
	handlers      []MessageHandler
	middlewares   []Middleware
	contextTokens *store.ContextStore
	cursorStore   *store.CursorStore
	stopped       bool
	mu            sync.Mutex
	cancelPoll    context.CancelFunc
	hooks         LifecycleHooks
	logger        func(level, msg string)
}

// New creates a new Bot instance.
func New(opts ...Options) *Bot {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.BaseURL == "" {
		o.BaseURL = protocol.DefaultBaseURL
	}
	if o.CredPath == "" && o.AccountID != "" {
		o.CredPath = filepath.Join(store.AccountStateDir(o.AccountID), "credentials.json")
	}
	if o.ContextTokenPath == "" && o.AccountID != "" {
		o.ContextTokenPath = filepath.Join(store.AccountStateDir(o.AccountID), "context_tokens.json")
	}
	if o.CursorPath == "" && o.AccountID != "" {
		o.CursorPath = filepath.Join(store.AccountStateDir(o.AccountID), "cursor.json")
	}
	client := protocol.NewClient()
	client.BotAgent = protocol.SanitizeBotAgent(o.BotAgent)
	client.RouteTag = o.RouteTag
	return &Bot{
		opts:          o,
		client:        client,
		sessionGuard:  session.NewGuard(),
		contextTokens: store.NewContextStore(o.AccountID, o.ContextTokenPath),
		cursorStore:   store.NewCursorStore(o.AccountID, o.CursorPath),
		hooks:         LifecycleHooks{},
	}
}

// Login performs QR code login or loads stored credentials.
func (b *Bot) Login(ctx context.Context, force bool) (*Credentials, error) {
	creds, err := auth.Login(ctx, b.client, auth.LoginOptions{
		BaseURL:      b.opts.BaseURL,
		CredPath:     b.opts.CredPath,
		Force:        force,
		OnQRURL:      b.opts.OnQRURL,
		OnScanned:    b.opts.OnScanned,
		OnExpired:    b.opts.OnExpired,
		OnVerifyCode: b.opts.OnVerifyCode,
	})
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	b.creds = creds
	b.opts.BaseURL = creds.BaseURL
	b.configCache = config.NewCache(config.APIOpts{
		BaseURL: creds.BaseURL,
		Token:   creds.Token,
		Client:  b.client,
	})
	b.mu.Unlock()

	if loadErr := b.contextTokens.Load(); loadErr != nil {
		b.log("warn", "Failed to load context tokens: %v", loadErr)
	}

	b.log("info", "Logged in as %s", creds.UserID)

	public := &Credentials{
		Token:     creds.Token,
		BaseURL:   creds.BaseURL,
		AccountID: creds.AccountID,
		UserID:    creds.UserID,
		SavedAt:   creds.SavedAt,
	}
	if err := b.hooks.AfterLogin.Run(public); err != nil {
		b.log("warn", "AfterLogin hook failed: %v", err)
	}
	return public, nil
}

// OnMessage registers a message handler.
func (b *Bot) OnMessage(handler MessageHandler) {
	b.handlers = append(b.handlers, handler)
}

// Use adds a middleware to the incoming message pipeline.
func (b *Bot) Use(mw Middleware) {
	b.middlewares = append(b.middlewares, mw)
}

// Hooks returns the bot's lifecycle hook registry for extension.
func (b *Bot) Hooks() *LifecycleHooks {
	return &b.hooks
}

// Reply sends a text reply to an incoming message.
func (b *Bot) Reply(ctx context.Context, msg *IncomingMessage, text string) error {
	if err := b.contextTokens.Set(msg.UserID, msg.ContextToken); err != nil {
		b.log("warn", "failed to persist context token: %v", err)
	}
	if err := b.sendText(ctx, msg.UserID, text, msg.ContextToken); err != nil {
		b.notifyError(ctx, msg.UserID, msg.ContextToken, err)
		return err
	}
	return nil
}

// Send sends a text message to a user (requires prior context_token).
func (b *Bot) Send(ctx context.Context, userID, text string) error {
	ct := b.contextTokens.Get(userID)
	if ct == "" {
		return fmt.Errorf("no context_token for user %s", userID)
	}
	if err := b.sendText(ctx, userID, text, ct); err != nil {
		b.notifyError(ctx, userID, ct, err)
		return err
	}
	return nil
}

// SendTyping shows the "typing..." indicator.
func (b *Bot) SendTyping(ctx context.Context, userID string) error {
	ct := b.contextTokens.Get(userID)
	if ct == "" {
		return fmt.Errorf("no context_token for user %s", userID)
	}
	creds := b.getCreds()
	cfg, err := b.configCache.GetForUser(ctx, userID, ct)
	if err != nil {
		return err
	}
	if cfg.TypingTicket == "" {
		return nil
	}
	return b.client.SendTyping(ctx, creds.BaseURL, creds.Token, userID, cfg.TypingTicket, 1)
}

// StopTyping cancels the "typing..." indicator.
func (b *Bot) StopTyping(ctx context.Context, userID string) error {
	ct := b.contextTokens.Get(userID)
	if ct == "" {
		return nil
	}
	creds := b.getCreds()
	cfg, err := b.configCache.GetForUser(ctx, userID, ct)
	if err != nil {
		return err
	}
	if cfg.TypingTicket == "" {
		return nil
	}
	return b.client.SendTyping(ctx, creds.BaseURL, creds.Token, userID, cfg.TypingTicket, 2)
}

// SendContent describes what to send. Use one of:
//   - SendText("Hello!")
//   - SendImage(data)
//   - SendImageURL("https://example.com/a.png")
//   - SendVideo(data)
//   - SendVideoURL("https://example.com/a.mp4")
//   - SendFile(data, "report.pdf")
//   - SendFileURL("https://example.com/report.pdf", "report.pdf")
type SendContent struct {
	Text     string
	Image    []byte
	Video    []byte
	File     []byte
	FileName string
	Caption  string
	ImageURL string
	VideoURL string
	FileURL  string
}

// resolveRemote downloads remote media when ImageURL/VideoURL/FileURL is set,
// returning a SendContent backed by local bytes.
func (content SendContent) resolveRemote(ctx context.Context) (SendContent, error) {
	if content.ImageURL != "" {
		data, _, err := remote.Download(ctx, content.ImageURL)
		if err != nil {
			return content, fmt.Errorf("download image: %w", err)
		}
		content.Image = data
		content.ImageURL = ""
	}
	if content.VideoURL != "" {
		data, _, err := remote.Download(ctx, content.VideoURL)
		if err != nil {
			return content, fmt.Errorf("download video: %w", err)
		}
		content.Video = data
		content.VideoURL = ""
	}
	if content.FileURL != "" {
		data, name, err := remote.Download(ctx, content.FileURL)
		if err != nil {
			return content, fmt.Errorf("download file: %w", err)
		}
		content.File = data
		content.FileURL = ""
		if content.FileName == "" {
			content.FileName = name
		}
	}
	return content, nil
}

// SendText creates a text SendContent.
func SendText(text string) SendContent { return SendContent{Text: text} }

// SendImage creates an image SendContent.
func SendImage(data []byte) SendContent { return SendContent{Image: data} }

// SendImageURL creates an image SendContent from a remote URL.
func SendImageURL(url string) SendContent { return SendContent{ImageURL: url} }

// SendVideo creates a video SendContent.
func SendVideo(data []byte) SendContent { return SendContent{Video: data} }

// SendVideoURL creates a video SendContent from a remote URL.
func SendVideoURL(url string) SendContent { return SendContent{VideoURL: url} }

// SendFile creates a file SendContent.
func SendFile(data []byte, fileName string) SendContent {
	return SendContent{File: data, FileName: fileName}
}

// SendFileURL creates a file SendContent from a remote URL.
func SendFileURL(url, fileName string) SendContent {
	return SendContent{FileURL: url, FileName: fileName}
}

// ReplyContent replies with any content type.
func (b *Bot) ReplyContent(ctx context.Context, msg *IncomingMessage, content SendContent) error {
	if err := b.contextTokens.Set(msg.UserID, msg.ContextToken); err != nil {
		b.log("warn", "failed to persist context token: %v", err)
	}
	resolved, err := content.resolveRemote(ctx)
	if err != nil {
		return err
	}
	if err := b.sendContent(ctx, msg.UserID, msg.ContextToken, resolved); err != nil {
		b.notifyError(ctx, msg.UserID, msg.ContextToken, err)
		return err
	}
	return nil
}

// SendMedia sends any content type to a user.
func (b *Bot) SendMedia(ctx context.Context, userID string, content SendContent) error {
	ct := b.contextTokens.Get(userID)
	if ct == "" {
		return fmt.Errorf("no context_token for user %s", userID)
	}
	resolved, err := content.resolveRemote(ctx)
	if err != nil {
		return err
	}
	if err := b.sendContent(ctx, userID, ct, resolved); err != nil {
		b.notifyError(ctx, userID, ct, err)
		return err
	}
	return nil
}

// Download downloads media from an incoming message.
// Returns nil if the message has no media. Priority: image > file > video > voice.
func (b *Bot) Download(ctx context.Context, msg *IncomingMessage) (*DownloadedMedia, error) {
	if len(msg.Images) > 0 && msg.Images[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Images[0].Media, msg.Images[0].AESKey)
		if err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: "image"}, nil
	}

	if len(msg.Files) > 0 && msg.Files[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Files[0].Media, "")
		if err != nil {
			return nil, err
		}
		name := msg.Files[0].FileName
		if name == "" {
			name = "file.bin"
		}
		return &DownloadedMedia{Data: data, Type: "file", FileName: name}, nil
	}

	if len(msg.Videos) > 0 && msg.Videos[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Videos[0].Media, "")
		if err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: "video"}, nil
	}

	if len(msg.Voices) > 0 && msg.Voices[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Voices[0].Media, "")
		if err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: "voice", Format: "silk"}, nil
	}

	return nil, nil
}

// DownloadRaw downloads and decrypts a raw CDN media reference.
func (b *Bot) DownloadRaw(ctx context.Context, media *CDNMedia, aeskeyOverride string) ([]byte, error) {
	return b.cdnDownload(ctx, media, aeskeyOverride)
}

// Upload uploads data to WeChat CDN without sending a message.
func (b *Bot) Upload(ctx context.Context, data []byte, userID string, mediaType int) (*UploadResult, error) {
	creds := b.getCreds()
	if creds == nil {
		return nil, fmt.Errorf("not logged in; call Login() first")
	}
	return b.cdnUpload(ctx, creds, data, userID, mediaType)
}

// Run starts the long-poll loop. Blocks until Stop() is called or context is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	creds := b.getCreds()
	if creds == nil {
		return fmt.Errorf("not logged in; call Login() first")
	}

	b.mu.Lock()
	b.stopped = false
	pollCtx, cancel := context.WithCancel(ctx)
	b.cancelPoll = cancel
	b.mu.Unlock()

	b.log("info", "Long-poll loop started")
	if loadErr := b.cursorStore.Load(); loadErr != nil {
		b.log("warn", "Failed to load cursor: %v", loadErr)
	}

	if err := b.hooks.BeforeLogin.Run(&Credentials{
		Token:     creds.Token,
		BaseURL:   creds.BaseURL,
		AccountID: creds.AccountID,
		UserID:    creds.UserID,
		SavedAt:   creds.SavedAt,
	}); err != nil {
		b.log("warn", "BeforeLogin hook failed: %v", err)
	}
	if err := b.client.NotifyStart(pollCtx, creds.BaseURL, creds.Token); err != nil {
		b.log("warn", "NotifyStart failed: %v", err)
	}
	defer func() {
		stopCreds := b.getCreds()
		if stopCreds == nil {
			return
		}
		if stopErr := b.client.NotifyStop(context.Background(), stopCreds.BaseURL, stopCreds.Token); stopErr != nil {
			b.log("warn", "NotifyStop failed: %v", stopErr)
		}
	}()

	retryDelay := time.Second
	pollTimeout := 45 * time.Second

	for {
		select {
		case <-pollCtx.Done():
			b.log("info", "Long-poll loop stopped")
			return nil
		default:
		}

		if remaining := b.sessionGuard.Remaining(); remaining > 0 {
			b.log("warn", "Session paused, waiting %v", remaining.Round(time.Second))
			timer := time.NewTimer(remaining)
			select {
			case <-pollCtx.Done():
				timer.Stop()
				b.log("info", "Long-poll loop stopped")
				return nil
			case <-timer.C:
				continue
			}
		}

		creds = b.getCreds()
		updates, err := b.client.GetUpdates(pollCtx, creds.BaseURL, creds.Token, b.cursorStore.Get(), pollTimeout)
		if err != nil {
			if pollCtx.Err() != nil {
				b.log("info", "Long-poll loop stopped")
				return nil
			}

			apiErr, isAPI := err.(*protocol.APIError)
			if isAPI && apiErr.IsSessionExpired() {
				b.log("warn", "Session expired — pausing for %v", session.PauseDuration)
				b.sessionGuard.Pause()
				retryDelay = time.Second
				continue
			}

			b.reportError(err)
			time.Sleep(retryDelay)
			retryDelay = min(retryDelay*2, 10*time.Second)
			continue
		}

		if updates.GetUpdatesBuf != "" {
			if saveErr := b.cursorStore.Set(updates.GetUpdatesBuf); saveErr != nil {
				b.log("warn", "Failed to save cursor: %v", saveErr)
			}
		}
		if updates.LongPollingTimeoutMs > 0 {
			pollTimeout = time.Duration(updates.LongPollingTimeoutMs) * time.Millisecond
		}
		retryDelay = time.Second

		for _, rawMsg := range updates.Msgs {
			var wire WireMessage
			if err := json.Unmarshal(rawMsg, &wire); err != nil {
				continue
			}
			b.rememberContext(&wire)
			incoming := b.parseMessage(&wire)
			if incoming == nil {
				continue
			}
			if err := b.hooks.AfterReceive.Run(incoming); err != nil {
				b.log("warn", "AfterReceive hook failed: %v", err)
				continue
			}
			if !b.runMiddleware(incoming) {
				continue
			}
			for _, h := range b.handlers {
				h(incoming)
			}
		}
	}
}

// Stop gracefully stops the poll loop.
func (b *Bot) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	if b.cancelPoll != nil {
		b.cancelPoll()
	}
}

// --- internal ---

func (b *Bot) sendContent(ctx context.Context, userID, contextToken string, content SendContent) error {
	if err := b.hooks.BeforeSend.Run(&content); err != nil {
		return fmt.Errorf("BeforeSend hook failed: %w", err)
	}

	// Text-only path.
	if content.Text != "" {
		return b.sendText(ctx, userID, content.Text, contextToken)
	}

	creds := b.getCreds()
	if creds == nil {
		return fmt.Errorf("not logged in; call Login() first")
	}

	// Send caption as a separate text message first, then send the media.
	if content.Caption != "" {
		if err := b.sendText(ctx, userID, content.Caption, contextToken); err != nil {
			return err
		}
	}

	// Image
	if content.Image != nil {
		result, err := b.cdnUpload(ctx, creds, content.Image, userID, int(MediaImage))
		if err != nil {
			return err
		}
		msg := protocol.BuildMediaMessage(userID, contextToken, []map[string]interface{}{{
			"type": 2, "image_item": map[string]interface{}{
				"media":    cdnMediaMap(&result.Media),
				"mid_size": result.EncryptedFileSize,
			},
		}})
		return b.client.SendMessage(ctx, creds.BaseURL, creds.Token, msg)
	}

	// Video
	if content.Video != nil {
		result, err := b.cdnUpload(ctx, creds, content.Video, userID, int(MediaVideo))
		if err != nil {
			return err
		}
		msg := protocol.BuildMediaMessage(userID, contextToken, []map[string]interface{}{{
			"type": 5, "video_item": map[string]interface{}{
				"media":      cdnMediaMap(&result.Media),
				"video_size": result.EncryptedFileSize,
			},
		}})
		return b.client.SendMessage(ctx, creds.BaseURL, creds.Token, msg)
	}

	// File (auto-route by extension)
	if content.File != nil {
		fileName := content.FileName
		if fileName == "" {
			fileName = "file.bin"
		}
		cat := categorizeByExtension(fileName)
		if cat == "image" {
			return b.sendContent(ctx, userID, contextToken, SendContent{Image: content.File})
		}
		if cat == "video" {
			return b.sendContent(ctx, userID, contextToken, SendContent{Video: content.File})
		}
		// Generic file
		result, err := b.cdnUpload(ctx, creds, content.File, userID, int(MediaFile))
		if err != nil {
			return err
		}
		msg := protocol.BuildMediaMessage(userID, contextToken, []map[string]interface{}{{
			"type": 4, "file_item": map[string]interface{}{
				"media":     cdnMediaMap(&result.Media),
				"file_name": fileName,
				"len":       strconv.Itoa(len(content.File)),
			},
		}})
		return b.client.SendMessage(ctx, creds.BaseURL, creds.Token, msg)
	}

	// Caption-only is valid: we already sent it above.
	if content.Caption != "" {
		return nil
	}

	return fmt.Errorf("empty SendContent")
}

func (b *Bot) cdnDownload(ctx context.Context, media *CDNMedia, aeskeyOverride string) ([]byte, error) {
	downloadURL := fmt.Sprintf("%s/download?encrypted_query_param=%s",
		protocol.CDNBaseURL, url.QueryEscape(media.EncryptQueryParam))

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdn download request: %w", err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cdn download failed: HTTP %d", resp.StatusCode)
	}

	ciphertext, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cdn download read: %w", err)
	}

	keySource := aeskeyOverride
	if keySource == "" {
		keySource = media.AESKey
	}
	if keySource == "" {
		return nil, fmt.Errorf("no AES key available for decryption")
	}

	aesKey, err := crypto.DecodeAESKey(keySource)
	if err != nil {
		return nil, fmt.Errorf("decode aes key: %w", err)
	}

	return crypto.DecryptAESECB(ciphertext, aesKey)
}

func (b *Bot) cdnUpload(ctx context.Context, creds *auth.Credentials, data []byte, userID string, mediaType int) (*UploadResult, error) {
	aesKey, err := crypto.GenerateAESKey()
	if err != nil {
		return nil, fmt.Errorf("generate aes key: %w", err)
	}
	ciphertext, err := crypto.EncryptAESECB(data, aesKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	var fileKeyBuf [16]byte
	if _, err := rand.Read(fileKeyBuf[:]); err != nil {
		return nil, fmt.Errorf("generate file key: %w", err)
	}
	fileKey := hex.EncodeToString(fileKeyBuf[:])

	rawMD5 := md5.Sum(data)
	rawMD5Hex := hex.EncodeToString(rawMD5[:])

	uploadResp, err := b.client.GetUploadURL(ctx, creds.BaseURL, creds.Token, protocol.GetUploadURLRequest{
		FileKey:     fileKey,
		MediaType:   mediaType,
		ToUserID:    userID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5Hex,
		FileSize:    len(ciphertext),
		NoNeedThumb: true,
		AESKey:      crypto.EncodeAESKeyHex(aesKey),
	})
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", err)
	}
	if uploadResp.UploadParam == "" {
		return nil, fmt.Errorf("getuploadurl did not return upload_param")
	}

	uploadURL := fmt.Sprintf("%s/upload?encrypted_query_param=%s&filekey=%s",
		protocol.CDNBaseURL,
		url.QueryEscape(uploadResp.UploadParam),
		url.QueryEscape(fileKey))

	req, err := http.NewRequestWithContext(ctx, "POST", uploadURL, bytes.NewReader(ciphertext))
	if err != nil {
		return nil, fmt.Errorf("cdn upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errMsg := resp.Header.Get("x-error-message")
		if errMsg == "" {
			errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("cdn upload failed: %s", errMsg)
	}

	encryptQueryParam := resp.Header.Get("x-encrypted-param")
	if encryptQueryParam == "" {
		return nil, fmt.Errorf("cdn upload succeeded but x-encrypted-param header missing")
	}

	return &UploadResult{
		Media: CDNMedia{
			EncryptQueryParam: encryptQueryParam,
			AESKey:            crypto.EncodeAESKeyBase64(aesKey),
			EncryptType:       1,
		},
		AESKey:            aesKey,
		EncryptedFileSize: len(ciphertext),
	}, nil
}

func cdnMediaMap(m *CDNMedia) map[string]interface{} {
	return map[string]interface{}{
		"encrypt_query_param": m.EncryptQueryParam,
		"aes_key":             m.AESKey,
		"encrypt_type":        m.EncryptType,
	}
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".svg": true}
var videoExts = map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".mkv": true, ".avi": true}

func categorizeByExtension(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if imageExts[ext] {
		return "image"
	}
	if videoExts[ext] {
		return "video"
	}
	return "file"
}

func (b *Bot) sendText(ctx context.Context, userID, text, contextToken string) error {
	creds := b.getCreds()
	if b.opts.StripMarkdown {
		text = markdown.StripMarkdown(text)
	}
	chunks := chunkText(text, 4000)
	for _, chunk := range chunks {
		msg := protocol.BuildTextMessage(userID, contextToken, chunk)
		if err := b.client.SendMessage(ctx, creds.BaseURL, creds.Token, msg); err != nil {
			return err
		}
	}
	return nil
}

// notifyError sends a short error notice to the user when NotifyErrors is enabled.
// Errors are best-effort; failures to send the notice are logged but not returned.
func (b *Bot) notifyError(ctx context.Context, userID, contextToken string, err error) {
	if !b.opts.NotifyErrors {
		return
	}
	creds := b.getCreds()
	if creds == nil {
		return
	}
	msg := "⚠️ 消息发送失败，请稍后重试。"
	if e := b.client.SendMessage(ctx, creds.BaseURL, creds.Token, protocol.BuildTextMessage(userID, contextToken, msg)); e != nil {
		b.log("warn", "failed to send error notice: %v", e)
	}
}

func (b *Bot) rememberContext(wire *WireMessage) {
	userID := wire.FromUserID
	if wire.MessageType == MessageTypeBot {
		userID = wire.ToUserID
	}
	if userID != "" && wire.ContextToken != "" {
		if err := b.contextTokens.Set(userID, wire.ContextToken); err != nil {
			b.log("warn", "failed to persist context token: %v", err)
		}
	}
}

func (b *Bot) parseMessage(wire *WireMessage) *IncomingMessage {
	if wire.MessageType != MessageTypeUser {
		return nil
	}

	msg := &IncomingMessage{
		UserID:       wire.FromUserID,
		Text:         extractText(wire.ItemList),
		Type:         detectType(wire.ItemList),
		Timestamp:    time.UnixMilli(wire.CreateTimeMs),
		Raw:          wire,
		ContextToken: wire.ContextToken,
	}

	for _, item := range wire.ItemList {
		if item.ImageItem != nil {
			msg.Images = append(msg.Images, ImageContent{
				Media: item.ImageItem.Media, ThumbMedia: item.ImageItem.ThumbMedia,
				AESKey: item.ImageItem.AESKey, URL: item.ImageItem.URL,
				Width: item.ImageItem.ThumbWidth, Height: item.ImageItem.ThumbHeight,
			})
		}
		if item.VoiceItem != nil {
			msg.Voices = append(msg.Voices, VoiceContent{
				Media: item.VoiceItem.Media, Text: item.VoiceItem.Text,
				DurationMs: item.VoiceItem.Playtime, EncodeType: item.VoiceItem.EncodeType,
			})
		}
		if item.FileItem != nil {
			size, _ := strconv.ParseInt(item.FileItem.Len, 10, 64)
			msg.Files = append(msg.Files, FileContent{
				Media: item.FileItem.Media, FileName: item.FileItem.FileName,
				MD5: item.FileItem.MD5, Size: size,
			})
		}
		if item.VideoItem != nil {
			msg.Videos = append(msg.Videos, VideoContent{
				Media: item.VideoItem.Media, ThumbMedia: item.VideoItem.ThumbMedia,
				DurationMs: item.VideoItem.PlayLength,
			})
		}
		if item.RefMsg != nil {
			q := &QuotedMessage{Title: item.RefMsg.Title}
			if item.RefMsg.MessageItem != nil && item.RefMsg.MessageItem.TextItem != nil {
				q.Text = item.RefMsg.MessageItem.TextItem.Text
			}
			msg.QuotedMessage = q
		}
	}

	return msg
}

func (b *Bot) getCreds() *auth.Credentials {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.creds
}

func (b *Bot) runMiddleware(msg *IncomingMessage) bool {
	for _, mw := range b.middlewares {
		if mw != nil && !mw(msg) {
			return false
		}
	}
	return true
}

func (b *Bot) reportError(err error) {
	b.log("error", "%v", err)
	if b.opts.OnError != nil {
		b.opts.OnError(err)
	}
}

func (b *Bot) log(level, format string, args ...interface{}) {
	if b.opts.LogLevel == "silent" {
		return
	}
	fmt.Fprintf(os.Stderr, "[wechatbot] %s\n", fmt.Sprintf(format, args...))
}

// SetLogger replaces the default stderr logger with a custom implementation.
func (b *Bot) SetLogger(fn func(level, msg string)) {
	if fn == nil {
		return
	}
	b.mu.Lock()
	b.logger = fn
	b.mu.Unlock()
}

func (b *Bot) doLog(level, format string, args ...interface{}) {
	b.mu.Lock()
	logger := b.logger
	b.mu.Unlock()
	if logger != nil {
		logger(level, fmt.Sprintf(format, args...))
		return
	}
	b.log(level, format, args...)
}

func detectType(items []MessageItem) ContentType {
	if len(items) == 0 {
		return ContentText
	}
	switch items[0].Type {
	case ItemImage:
		return ContentImage
	case ItemVoice:
		return ContentVoice
	case ItemFile:
		return ContentFile
	case ItemVideo:
		return ContentVideo
	default:
		return ContentText
	}
}

func extractText(items []MessageItem) string {
	var parts []string
	for _, item := range items {
		switch item.Type {
		case ItemText:
			if item.TextItem != nil {
				parts = append(parts, item.TextItem.Text)
			}
		case ItemImage:
			if item.ImageItem != nil && item.ImageItem.URL != "" {
				parts = append(parts, item.ImageItem.URL)
			} else {
				parts = append(parts, "[image]")
			}
		case ItemVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				parts = append(parts, item.VoiceItem.Text)
			} else {
				parts = append(parts, "[voice]")
			}
		case ItemFile:
			if item.FileItem != nil && item.FileItem.FileName != "" {
				parts = append(parts, item.FileItem.FileName)
			} else {
				parts = append(parts, "[file]")
			}
		case ItemVideo:
			parts = append(parts, "[video]")
		}
	}
	return strings.Join(parts, "\n")
}

func chunkText(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		cut := limit
		if idx := strings.LastIndex(text[:limit], "\n\n"); idx > limit*3/10 {
			cut = idx + 2
		} else if idx := strings.LastIndex(text[:limit], "\n"); idx > limit*3/10 {
			cut = idx + 1
		} else if idx := strings.LastIndex(text[:limit], " "); idx > limit*3/10 {
			cut = idx + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
