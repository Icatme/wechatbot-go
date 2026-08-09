package wechatbot

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime/debug"
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
	"github.com/Icatme/wechatbot-go/internal/store"
	"github.com/Icatme/wechatbot-go/internal/thumb"
	botlog "github.com/Icatme/wechatbot-go/log"
)

// Options configures a Bot instance.
type Options struct {
	BaseURL               string
	AccountID             string // optional account identifier for multi-bot isolation
	CredPath              string
	ContextTokenPath      string
	CursorPath            string
	ReplayPath            string // optional persistent replay-dedupe state path
	BotAgent              string // UA-style, e.g. "MyBot/1.2.0"
	RouteTag              string // sent as SKRouteTag header
	StripMarkdown         bool   // strip markdown from outbound text
	NotifyErrors          bool   // automatically notify user on send failure
	LogLevel              string // "debug", "info", "warn", "error", "silent"
	MaxConcurrentHandlers int    // maximum concurrent conversations; defaults to 4 and caps at 256
	Logger                *botlog.Logger
	OnQRURL               func(url string)
	OnScanned             func()
	OnExpired             func()
	OnVerifyCode          func() (string, error)
	OnError               func(err error)
}

// Bot is the main WeChat bot client.
type Bot struct {
	opts               Options
	client             *protocol.Client
	creds              *auth.Credentials
	configCache        *config.Cache
	reauth             *reauthState
	sessionGeneration  uint64
	sessionInitialized bool
	sessionContext     context.Context
	cancelSession      context.CancelFunc
	handler            MessageHandler
	middlewares        []Middleware
	contextTokens      *store.ContextStore
	cursorStore        *store.CursorStore
	replayStore        *store.ReplayStore
	running            bool
	mu                 sync.Mutex
	loginMu            sync.Mutex
	sessionMu          sync.Mutex
	requestMu          sync.RWMutex
	cancelPoll         context.CancelFunc
	hooks              LifecycleHooks
	logger             loggerAdapter
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
	if o.ReplayPath == "" && o.AccountID != "" {
		o.ReplayPath = filepath.Join(store.AccountStateDir(o.AccountID), "replay_dedupe.json")
	}
	client := protocol.NewClient()
	client.BotAgent = protocol.SanitizeBotAgent(o.BotAgent)
	client.RouteTag = o.RouteTag

	var logger loggerAdapter
	if o.Logger != nil {
		logger = o.Logger
	} else {
		logger = newDefaultLogger(o.LogLevel)
	}

	return &Bot{
		opts:          o,
		client:        client,
		contextTokens: store.NewContextStore(o.AccountID, o.ContextTokenPath),
		cursorStore:   store.NewCursorStore(o.AccountID, o.CursorPath),
		replayStore:   store.NewReplayStore(o.AccountID, o.ReplayPath, store.DefaultReplayTTL),
		hooks:         LifecycleHooks{},
		logger:        logger,
	}
}

// Login performs QR code login or loads stored credentials.
func (b *Bot) Login(ctx context.Context, force bool) (*Credentials, error) {
	if force {
		return b.Reauthenticate(ctx)
	}
	b.mu.Lock()
	state := b.reauth
	creds := b.creds
	b.mu.Unlock()
	if state != nil {
		return nil, waitReauthState(state)
	}
	if creds != nil {
		return publicCredentials(creds), nil
	}
	if !b.loginMu.TryLock() {
		return nil, ErrLoginInProgress
	}
	defer b.loginMu.Unlock()
	if err := b.reauthError(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	creds = b.creds
	b.mu.Unlock()
	if creds != nil {
		return publicCredentials(creds), nil
	}
	return b.login(ctx, false, nil)
}

// Reauthenticate explicitly performs a fresh QR login without offering or
// accepting credentials that were invalidated by a -14 response.
func (b *Bot) Reauthenticate(ctx context.Context) (*Credentials, error) {
	if !b.loginMu.TryLock() {
		return nil, ErrLoginInProgress
	}
	defer b.loginMu.Unlock()

	state, err := b.prepareReauthentication()
	if err != nil {
		return nil, err
	}
	return b.login(ctx, true, state)
}

func (b *Bot) login(ctx context.Context, force bool, reauth *reauthState) (*Credentials, error) {
	invalidToken := ""
	expectedAccountID := b.opts.AccountID
	if reauth != nil {
		invalidToken = reauth.invalidToken
		if reauth.accountID != "" {
			expectedAccountID = reauth.accountID
		}
	}
	creds, err := auth.Login(ctx, b.client, auth.LoginOptions{
		BaseURL:      b.opts.BaseURL,
		CredPath:     b.opts.CredPath,
		Force:        force,
		InvalidToken: invalidToken,
		OnQRURL:      b.opts.OnQRURL,
		OnScanned:    b.opts.OnScanned,
		OnExpired:    b.opts.OnExpired,
		OnVerifyCode: b.opts.OnVerifyCode,
	})
	if err != nil {
		return nil, reauthenticationAttemptError(reauth, err)
	}
	if expectedAccountID != "" && creds.AccountID != expectedAccountID {
		cleanupErr := auth.ClearCredentials(b.opts.CredPath)
		return nil, reauthenticationAttemptError(reauth, errors.Join(
			fmt.Errorf("reauthenticated account %q does not match %q", creds.AccountID, expectedAccountID),
			cleanupErr,
		))
	}

	if err := b.hooks.BeforeLogin.Run(publicCredentials(creds)); err != nil {
		b.log("warn", "BeforeLogin hook failed: %v", err)
	}

	accountID := b.opts.AccountID
	if accountID == "" {
		accountID = creds.AccountID
	}
	contextTokens := b.contextTokens
	cursorStore := b.cursorStore
	replayStore := b.replayStore
	if reauth == nil && b.opts.ContextTokenPath == "" {
		contextTokens = store.NewContextStore(accountID, "")
	}
	if reauth == nil && b.opts.CursorPath == "" {
		cursorStore = store.NewCursorStore(accountID, "")
	}
	if reauth == nil && b.opts.ReplayPath == "" {
		replayStore = store.NewReplayStore(accountID, "", store.DefaultReplayTTL)
	}
	b.sessionMu.Lock()
	if force {
		if err := contextTokens.Clear(); err != nil {
			b.sessionMu.Unlock()
			cleanupErr := auth.ClearCredentials(b.opts.CredPath)
			return nil, reauthenticationAttemptError(reauth, errors.Join(fmt.Errorf("clear context tokens for reauthentication: %w", err), cleanupErr))
		}
	} else if err := contextTokens.Load(); err != nil {
		b.log("warn", "Failed to load context tokens: %v", err)
	}

	b.requestMu.Lock()
	b.mu.Lock()
	currentReauth := b.reauth
	if (reauth == nil && currentReauth != nil) || (reauth != nil && currentReauth != reauth) {
		b.mu.Unlock()
		b.requestMu.Unlock()
		b.sessionMu.Unlock()
		cleanupErr := auth.ClearCredentials(b.opts.CredPath)
		return nil, reauthenticationAttemptError(reauth, errors.Join(
			fmt.Errorf("login state changed while credentials were being acquired"),
			waitReauthState(currentReauth),
			cleanupErr,
		))
	}
	nextGeneration := b.sessionGeneration + 1
	nextSessionContext, cancelNextSession := context.WithCancel(context.Background())
	previousCancelSession := b.cancelSession
	configCache := b.newConfigCache(creds, nextGeneration)
	b.creds = creds
	b.configCache = configCache
	b.contextTokens = contextTokens
	b.cursorStore = cursorStore
	b.replayStore = replayStore
	b.opts.BaseURL = creds.BaseURL
	b.opts.AccountID = accountID
	b.sessionGeneration = nextGeneration
	b.sessionInitialized = true
	b.sessionContext = nextSessionContext
	b.cancelSession = cancelNextSession
	b.reauth = nil
	b.mu.Unlock()
	b.requestMu.Unlock()
	b.sessionMu.Unlock()
	if previousCancelSession != nil {
		previousCancelSession()
	}

	b.log("info", "Logged in as %s", creds.UserID)
	if err := b.hooks.AfterLogin.Run(publicCredentials(creds)); err != nil {
		b.log("warn", "AfterLogin hook failed: %v", err)
	}
	return publicCredentials(creds), nil
}

func reauthenticationAttemptError(state *reauthState, err error) error {
	if state == nil {
		return err
	}
	return errors.Join(err, waitReauthState(state))
}

func publicCredentials(creds *auth.Credentials) *Credentials {
	return &Credentials{
		Token:     creds.Token,
		BaseURL:   creds.BaseURL,
		AccountID: creds.AccountID,
		UserID:    creds.UserID,
		SavedAt:   creds.SavedAt,
	}
}

// Handle sets the single inbound message handler. Replacing the handler while
// Run is active takes effect on the next Run.
func (b *Bot) Handle(handler MessageHandler) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

// Use adds a middleware to the incoming message pipeline. Middleware added
// while Run is active takes effect on the next Run.
func (b *Bot) Use(mw Middleware) {
	b.mu.Lock()
	b.middlewares = append(b.middlewares, mw)
	b.mu.Unlock()
}

// Hooks returns the bot's lifecycle hook registry for extension.
func (b *Bot) Hooks() *LifecycleHooks {
	return &b.hooks
}

// Reply sends a text reply to an incoming message.
func (b *Bot) Reply(ctx context.Context, msg *IncomingMessage, text string) error {
	if msg == nil {
		return fmt.Errorf("inbound message is nil")
	}
	sessionGeneration, err := b.incomingSessionGeneration(msg)
	if err != nil {
		return err
	}
	if err := requireContextToken(msg.UserID, msg.ContextToken); err != nil {
		return err
	}
	if err := b.persistContextToken(sessionGeneration, msg.UserID, msg.ContextToken); err != nil {
		if errors.Is(err, ErrReauthRequired) {
			return err
		}
		b.log("warn", "failed to persist context token: %v", err)
	}
	if err := b.sendText(ctx, sessionGeneration, msg.UserID, text, msg.ContextToken); err != nil {
		b.notifyError(ctx, sessionGeneration, msg.UserID, msg.ContextToken, err)
		return err
	}
	return nil
}

// Send sends a text message to a user (requires prior context_token).
func (b *Bot) Send(ctx context.Context, userID, text string) error {
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if err := requireContextToken(userID, ct); err != nil {
		return err
	}
	if err := b.sendText(ctx, sessionGeneration, userID, text, ct); err != nil {
		b.notifyError(ctx, sessionGeneration, userID, ct, err)
		return err
	}
	return nil
}

// SendTyping shows the "typing..." indicator.
func (b *Bot) SendTyping(ctx context.Context, userID string) error {
	creds, configCache, sessionGeneration, err := b.readyConfig(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if err := requireContextToken(userID, ct); err != nil {
		return err
	}
	cfg, err := configCache.GetForUser(ctx, userID, ct)
	if err != nil {
		return b.handleAuthenticatedErrorForSession(sessionGeneration, creds.Token, err)
	}
	creds, err = b.readySession(sessionGeneration)
	if err != nil {
		return err
	}
	if cfg.TypingTicket == "" {
		return nil
	}
	return b.sendTypingForSession(ctx, sessionGeneration, userID, cfg.TypingTicket, 1)
}

// StopTyping cancels the "typing..." indicator.
func (b *Bot) StopTyping(ctx context.Context, userID string) error {
	creds, configCache, sessionGeneration, err := b.readyConfig(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if ct == "" {
		return nil
	}
	cfg, err := configCache.GetForUser(ctx, userID, ct)
	if err != nil {
		return b.handleAuthenticatedErrorForSession(sessionGeneration, creds.Token, err)
	}
	creds, err = b.readySession(sessionGeneration)
	if err != nil {
		return err
	}
	if cfg.TypingTicket == "" {
		return nil
	}
	return b.sendTypingForSession(ctx, sessionGeneration, userID, cfg.TypingTicket, 2)
}

func (b *Bot) sendTypingForSession(ctx context.Context, sessionGeneration uint64, userID, ticket string, status int) error {
	token, sendErr := b.authenticatedRequest(ctx, sessionGeneration, func(requestContext context.Context, baseURL, token string) error {
		return b.client.SendTyping(requestContext, baseURL, token, userID, ticket, status)
	})
	if token == "" {
		return sendErr
	}
	if err := b.handleAuthenticatedErrorForSession(sessionGeneration, token, sendErr); err != nil {
		return err
	}
	_, err := b.readySession(sessionGeneration)
	return err
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
func (content SendContent) resolveRemote(ctx context.Context, httpClient *http.Client) (SendContent, error) {
	if content.ImageURL != "" {
		data, _, err := remote.DownloadWithClient(ctx, httpClient, content.ImageURL)
		if err != nil {
			return content, fmt.Errorf("download image: %w", err)
		}
		content.Image = data
		content.ImageURL = ""
	}
	if content.VideoURL != "" {
		data, _, err := remote.DownloadWithClient(ctx, httpClient, content.VideoURL)
		if err != nil {
			return content, fmt.Errorf("download video: %w", err)
		}
		content.Video = data
		content.VideoURL = ""
	}
	if content.FileURL != "" {
		data, name, err := remote.DownloadWithClient(ctx, httpClient, content.FileURL)
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
	if msg == nil {
		return fmt.Errorf("inbound message is nil")
	}
	sessionGeneration, err := b.incomingSessionGeneration(msg)
	if err != nil {
		return err
	}
	if err := requireContextToken(msg.UserID, msg.ContextToken); err != nil {
		return err
	}
	if err := b.persistContextToken(sessionGeneration, msg.UserID, msg.ContextToken); err != nil {
		if errors.Is(err, ErrReauthRequired) {
			return err
		}
		b.log("warn", "failed to persist context token: %v", err)
	}
	resolved, err := content.resolveRemote(ctx, b.client.HTTP)
	if err != nil {
		return err
	}
	if err := b.sendContent(ctx, sessionGeneration, msg.UserID, msg.ContextToken, resolved); err != nil {
		b.notifyError(ctx, sessionGeneration, msg.UserID, msg.ContextToken, err)
		return err
	}
	return nil
}

// SendMedia sends any content type to a user.
func (b *Bot) SendMedia(ctx context.Context, userID string, content SendContent) error {
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if err := requireContextToken(userID, ct); err != nil {
		return err
	}
	resolved, err := content.resolveRemote(ctx, b.client.HTTP)
	if err != nil {
		return err
	}
	if err := b.sendContent(ctx, sessionGeneration, userID, ct, resolved); err != nil {
		b.notifyError(ctx, sessionGeneration, userID, ct, err)
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
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return nil, err
	}
	return b.cdnUpload(ctx, sessionGeneration, data, userID, mediaType)
}

const updateQueueCapacity = 16

type updateBatch struct {
	messages          []json.RawMessage
	cursor            string
	sessionGeneration uint64
}

type authenticatedSession struct {
	creds      *auth.Credentials
	generation uint64
}

type handlerSessionGenerationKey struct{}

func (b *Bot) beginRun(ctx context.Context) (authenticatedSession, MessageHandler, context.Context, context.CancelFunc, error) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, ErrAlreadyRunning
	}
	if state := b.reauth; state != nil {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, waitReauthState(state)
	}
	if b.creds == nil {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, fmt.Errorf("%w; call Login() first", ErrNotLoggedIn)
	}
	if b.handler == nil {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, ErrNoMessageHandler
	}

	session := authenticatedSession{creds: b.creds, generation: b.sessionGeneration}
	b.sessionInitialized = true
	handler := b.handler
	middlewares := append([]Middleware(nil), b.middlewares...)
	pollCtx, cancel := context.WithCancel(ctx)
	b.running = true
	b.cancelPoll = cancel
	b.mu.Unlock()

	handler, err := composeHandler(handler, middlewares)
	if err != nil {
		b.finishRun(cancel)
		return authenticatedSession{}, nil, nil, nil, err
	}
	return session, handler, pollCtx, cancel, nil
}

func (b *Bot) finishRun(cancel context.CancelFunc) {
	cancel()
	b.mu.Lock()
	b.running = false
	b.cancelPoll = nil
	b.mu.Unlock()
}

// Run starts the long-poll loop. It blocks until Stop is called, ctx is
// cancelled, or a delivery requests retry. Only one Run may be active.
func (b *Bot) Run(ctx context.Context) (runErr error) {
	session, handler, pollCtx, cancel, err := b.beginRun(ctx)
	if err != nil {
		return err
	}
	defer b.finishRun(cancel)
	defer func() {
		runErr = b.normalizeSessionChangeError(session.generation, runErr)
	}()

	b.log("info", "Long-poll loop started")
	if loadErr := b.cursorStore.Load(); loadErr != nil {
		return fmt.Errorf("load cursor state: %w", loadErr)
	}
	if err := b.replayStore.Load(); err != nil {
		return fmt.Errorf("load replay state: %w", err)
	}
	notifyStartToken, notifyStartErr := b.authenticatedRequest(pollCtx, session.generation, func(requestContext context.Context, baseURL, token string) error {
		return b.client.NotifyStart(requestContext, baseURL, token)
	})
	if notifyStartToken == "" {
		return notifyStartErr
	}
	if err := notifyStartErr; err != nil {
		err = b.handleAuthenticatedErrorForSession(session.generation, notifyStartToken, err)
		if errors.Is(err, ErrReauthRequired) {
			return err
		}
		b.log("warn", "NotifyStart failed: %v", err)
	}
	defer func() {
		stopToken, stopErr := b.authenticatedRequest(context.Background(), session.generation, func(requestContext context.Context, baseURL, token string) error {
			return b.client.NotifyStop(requestContext, baseURL, token)
		})
		if stopToken == "" {
			return
		}
		if stopToken != session.creds.Token {
			return
		}
		if stopErr != nil {
			stopErr = b.handleAuthenticatedErrorForSession(session.generation, stopToken, stopErr)
			if errors.Is(stopErr, ErrReauthRequired) {
				runErr = errors.Join(runErr, stopErr)
				return
			}
			b.log("warn", "NotifyStop failed: %v", stopErr)
		}
	}()

	batches := make(chan updateBatch, updateQueueCapacity)
	processorErrCh := make(chan error, 1)
	processorDone := make(chan struct{})
	go func() {
		defer close(processorDone)
		if err := b.processUpdateBatches(pollCtx, handler, batches); err != nil {
			processorErrCh <- err
			cancel()
		}
	}()
	defer func() {
		cancel()
		<-processorDone
	}()

	processorErr := func() error {
		select {
		case err := <-processorErrCh:
			return fmt.Errorf("process updates: %w", err)
		default:
			return nil
		}
	}
	sessionErr := func() error {
		return b.sessionError(session.generation)
	}
	stopResult := func() error {
		cancel()
		<-processorDone
		result := processorErr()
		if err := sessionErr(); err != nil {
			result = errors.Join(result, err)
		}
		if result == nil {
			b.log("info", "Long-poll loop stopped")
		}
		return result
	}

	retryDelay := time.Second
	pollTimeout := 45 * time.Second
	pollCursor := b.cursorStore.Get()

	for {
		if err := processorErr(); err != nil {
			return errors.Join(err, sessionErr())
		}
		select {
		case <-pollCtx.Done():
			return stopResult()
		default:
		}

		updates, requestToken, err := authenticatedRequestResult(b, pollCtx, session.generation, func(requestContext context.Context, baseURL, token string) (*protocol.GetUpdatesResponse, error) {
			return b.client.GetUpdates(requestContext, baseURL, token, pollCursor, pollTimeout)
		})
		if requestToken == "" {
			return errors.Join(err, stopResult())
		}
		if err != nil {
			err = b.handleAuthenticatedErrorForSession(session.generation, requestToken, err)
			if errors.Is(err, ErrReauthRequired) {
				return errors.Join(err, stopResult())
			}
			if pollCtx.Err() != nil {
				return stopResult()
			}

			b.reportError(err)
			timer := time.NewTimer(retryDelay)
			select {
			case <-pollCtx.Done():
				timer.Stop()
				return stopResult()
			case <-timer.C:
			}
			retryDelay = min(retryDelay*2, 10*time.Second)
			continue
		}

		if updates.LongPollingTimeoutMs > 0 {
			pollTimeout = time.Duration(updates.LongPollingTimeoutMs) * time.Millisecond
		}
		retryDelay = time.Second

		nextCursor := pollCursor
		if updates.GetUpdatesBuf != "" {
			nextCursor = updates.GetUpdatesBuf
		}
		if len(updates.Msgs) > 0 || nextCursor != pollCursor {
			batch := updateBatch{messages: updates.Msgs, cursor: nextCursor, sessionGeneration: session.generation}
			select {
			case batches <- batch:
				pollCursor = nextCursor
			case err := <-processorErrCh:
				return errors.Join(fmt.Errorf("process updates: %w", err), sessionErr())
			case <-pollCtx.Done():
				return stopResult()
			}
		}
	}
}

func (b *Bot) processUpdateBatches(ctx context.Context, handler MessageHandler, batches <-chan updateBatch) error {
	dispatcher := newKeyedDispatcher(ctx, b, handler, b.opts.MaxConcurrentHandlers)
	for {
		select {
		case <-ctx.Done():
			return dispatcher.wait()
		case <-dispatcher.failure():
			return dispatcher.wait()
		case batch, ok := <-batches:
			if !ok {
				return dispatcher.drain()
			}
			if err := dispatcher.submit(batch); err != nil {
				return dispatcher.wait()
			}
		}
	}
}

func (b *Bot) processUpdateBatch(ctx context.Context, handler MessageHandler, batch updateBatch) error {
	for _, rawMsg := range batch.messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		wire, err := b.decodeWireMessage(rawMsg)
		if err != nil {
			return err
		}
		if err := b.processWireMessage(ctx, handler, batch.sessionGeneration, wire); err != nil {
			return err
		}
	}
	if batch.cursor != "" && batch.cursor != b.cursorStore.Get() {
		if err := b.cursorStore.Set(batch.cursor); err != nil {
			return fmt.Errorf("commit cursor: %w", err)
		}
	}
	return nil
}

func (b *Bot) decodeWireMessage(rawMsg json.RawMessage) (*WireMessage, error) {
	var wire WireMessage
	if err := json.Unmarshal(rawMsg, &wire); err != nil {
		return nil, fmt.Errorf("decode incoming message: %w", err)
	}
	return &wire, nil
}

func (b *Bot) processWireMessage(ctx context.Context, handler MessageHandler, sessionGeneration uint64, wire *WireMessage) error {
	if err := b.validateDeliverySession(sessionGeneration); err != nil {
		return err
	}
	keys := replayKeys(wire)
	// replayKeys orders aliases strongest-first. A weaker alias may collide on a
	// distinct delivery, so only the strongest identity present may suppress it.
	if len(keys) > 0 && b.replayStore.SeenAny(keys[0]) {
		b.log("debug", "Skipping replayed message with identity %s", keys[0])
		return nil
	}

	if err := b.rememberContextForSession(sessionGeneration, wire); err != nil {
		return err
	}
	incoming := b.parseMessage(wire)
	if incoming != nil {
		incoming.sessionGeneration = sessionGeneration
		incoming.sessionBound = true
		if err := b.hooks.AfterReceive.Run(incoming); err != nil {
			return fmt.Errorf("AfterReceive hook failed: %w", err)
		}
		handlerCtx := context.WithValue(ctx, handlerSessionGenerationKey{}, sessionGeneration)
		result := b.invokeHandler(handlerCtx, handler, incoming)
		if err := validateMessageResult(result); err != nil {
			return fmt.Errorf("handle message: %w", err)
		}
		if result.Action == MessageDrop && result.Err != nil {
			b.log("warn", "Message intentionally dropped: %v", result.Err)
		}
	}
	if len(keys) > 0 {
		if err := b.replayStore.CommitAll(keys...); err != nil {
			return fmt.Errorf("commit replay identities %v: %w", keys, err)
		}
	}
	return nil
}

func replayKeys(wire *WireMessage) []string {
	peer := peerUserID(wire)
	if peer == "" {
		return nil
	}

	prefix := "peer:" + strconv.Itoa(len(peer)) + ":" + peer + ":"
	keys := make([]string, 0, 3)
	if wire.MessageID != 0 {
		keys = append(keys, prefix+"message:"+strconv.FormatInt(wire.MessageID, 10))
	}
	if wire.ClientID != "" {
		keys = append(keys, prefix+"client:"+wire.ClientID)
	}
	if wire.Seq != 0 {
		keys = append(keys, prefix+"seq:"+strconv.FormatInt(wire.Seq, 10))
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

func peerUserID(wire *WireMessage) string {
	if wire == nil {
		return ""
	}
	if wire.MessageType == MessageTypeBot {
		return wire.ToUserID
	}
	if wire.MessageType == MessageTypeUser {
		return wire.FromUserID
	}
	return ""
}

// Stop gracefully stops the poll loop.
func (b *Bot) Stop() {
	b.mu.Lock()
	cancel := b.cancelPoll
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// --- internal ---

func requireContextToken(userID, contextToken string) error {
	if contextToken == "" {
		return fmt.Errorf("no context_token for user %s", userID)
	}
	return nil
}

func (b *Bot) readyCreds() (*auth.Credentials, error) {
	creds, _, err := b.readySessionSnapshot()
	return creds, err
}

func (b *Bot) readySessionSnapshot() (*auth.Credentials, uint64, error) {
	b.mu.Lock()
	state := b.reauth
	creds := b.creds
	generation := b.sessionGeneration
	b.mu.Unlock()
	if state != nil {
		return nil, 0, waitReauthState(state)
	}
	if creds == nil {
		return nil, 0, fmt.Errorf("%w; call Login() first", ErrNotLoggedIn)
	}
	return creds, generation, nil
}

func (b *Bot) readySession(generation uint64) (*auth.Credentials, error) {
	b.mu.Lock()
	state := b.reauth
	creds := b.creds
	currentGeneration := b.sessionGeneration
	b.mu.Unlock()
	if currentGeneration != generation {
		return nil, ErrSessionChanged
	}
	if state != nil {
		return nil, waitReauthState(state)
	}
	if creds == nil {
		return nil, fmt.Errorf("%w; call Login() first", ErrNotLoggedIn)
	}
	return creds, nil
}

func (b *Bot) beginAuthenticatedRequest(ctx context.Context, generation uint64) (*auth.Credentials, context.Context, func(), error) {
	b.requestMu.RLock()
	b.mu.Lock()
	state := b.reauth
	creds := b.creds
	currentGeneration := b.sessionGeneration
	sessionContext := b.sessionContext
	b.mu.Unlock()
	if currentGeneration != generation {
		b.requestMu.RUnlock()
		return nil, nil, nil, ErrSessionChanged
	}
	if state != nil {
		b.requestMu.RUnlock()
		return nil, nil, nil, waitReauthState(state)
	}
	if creds == nil {
		b.requestMu.RUnlock()
		return nil, nil, nil, fmt.Errorf("%w; call Login() first", ErrNotLoggedIn)
	}
	if sessionContext == nil {
		return creds, ctx, b.requestMu.RUnlock, nil
	}
	requestContext, cancelRequest := context.WithCancel(ctx)
	stopSessionCancel := context.AfterFunc(sessionContext, cancelRequest)
	if sessionContext.Err() != nil {
		cancelRequest()
	}
	finishRequest := func() {
		stopSessionCancel()
		cancelRequest()
		b.requestMu.RUnlock()
	}
	return creds, requestContext, finishRequest, nil
}

func (b *Bot) authenticatedRequest(ctx context.Context, generation uint64, call func(context.Context, string, string) error) (string, error) {
	creds, requestContext, finishRequest, err := b.beginAuthenticatedRequest(ctx, generation)
	if err != nil {
		return "", err
	}
	defer finishRequest()
	return creds.Token, call(requestContext, creds.BaseURL, creds.Token)
}

func authenticatedRequestResult[T any](b *Bot, ctx context.Context, generation uint64, call func(context.Context, string, string) (T, error)) (T, string, error) {
	var zero T
	creds, requestContext, finishRequest, err := b.beginAuthenticatedRequest(ctx, generation)
	if err != nil {
		return zero, "", err
	}
	defer finishRequest()
	result, err := call(requestContext, creds.BaseURL, creds.Token)
	return result, creds.Token, err
}

func (b *Bot) validateDeliverySession(generation uint64) error {
	b.mu.Lock()
	initialized := b.sessionInitialized
	b.mu.Unlock()
	if !initialized {
		return nil
	}
	_, err := b.readySession(generation)
	return err
}

func (b *Bot) readySessionForContext(ctx context.Context) (*auth.Credentials, uint64, error) {
	if ctx != nil {
		if generation, ok := ctx.Value(handlerSessionGenerationKey{}).(uint64); ok {
			creds, err := b.readySession(generation)
			return creds, generation, err
		}
	}
	return b.readySessionSnapshot()
}

func (b *Bot) incomingSessionGeneration(msg *IncomingMessage) (uint64, error) {
	if msg.sessionBound {
		if _, err := b.readySession(msg.sessionGeneration); err != nil {
			return 0, err
		}
		return msg.sessionGeneration, nil
	}
	_, generation, err := b.readySessionSnapshot()
	return generation, err
}

type generationConfigProvider struct {
	bot        *Bot
	generation uint64
}

func (p generationConfigProvider) GetConfig(ctx context.Context, _, _ string, userID, contextToken string) (*protocol.GetConfigResponse, error) {
	resp, _, err := authenticatedRequestResult(p.bot, ctx, p.generation, func(requestContext context.Context, baseURL, token string) (*protocol.GetConfigResponse, error) {
		return p.bot.client.GetConfig(requestContext, baseURL, token, userID, contextToken)
	})
	return resp, err
}

func (b *Bot) newConfigCache(creds *auth.Credentials, generation uint64) *config.Cache {
	return config.NewCache(config.APIOpts{
		BaseURL: creds.BaseURL,
		Token:   creds.Token,
		Client: generationConfigProvider{
			bot:        b,
			generation: generation,
		},
	})
}

func (b *Bot) readyConfig(ctx context.Context) (*auth.Credentials, *config.Cache, uint64, error) {
	expectedGeneration, bound := uint64(0), false
	if ctx != nil {
		expectedGeneration, bound = ctx.Value(handlerSessionGenerationKey{}).(uint64)
	}
	b.mu.Lock()
	state := b.reauth
	creds := b.creds
	configCache := b.configCache
	generation := b.sessionGeneration
	b.mu.Unlock()
	if bound && expectedGeneration != generation {
		return nil, nil, 0, ErrSessionChanged
	}
	if state != nil {
		return nil, nil, 0, waitReauthState(state)
	}
	if creds == nil || configCache == nil {
		return nil, nil, 0, fmt.Errorf("%w; call Login() first", ErrNotLoggedIn)
	}
	return creds, configCache, generation, nil
}

func (b *Bot) sendContent(ctx context.Context, sessionGeneration uint64, userID, contextToken string, content SendContent) error {
	if err := requireContextToken(userID, contextToken); err != nil {
		return err
	}

	// Text-only path.
	if content.Text != "" {
		return b.sendText(ctx, sessionGeneration, userID, content.Text, contextToken)
	}

	if _, err := b.readySession(sessionGeneration); err != nil {
		return err
	}

	// Send caption as a separate text message first, then send the media.
	if content.Caption != "" {
		if err := b.sendText(ctx, sessionGeneration, userID, content.Caption, contextToken); err != nil {
			return err
		}
	}

	// Image
	if content.Image != nil {
		thumbData, _ := thumb.FromImage(content.Image)
		if thumbData == nil {
			thumbData = thumb.Placeholder()
		}
		result, err := b.cdnUploadWithThumb(ctx, sessionGeneration, content.Image, thumbData, userID, int(MediaImage))
		if err != nil {
			return err
		}
		imageItem := &ImageItem{
			Media:   &result.Media,
			MidSize: int64(result.EncryptedFileSize),
		}
		if result.ThumbMedia.EncryptQueryParam != "" {
			imageItem.ThumbMedia = &result.ThumbMedia
		}
		_, err = b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type:      ItemImage,
			ImageItem: imageItem,
		}})
		return err
	}

	// Video
	if content.Video != nil {
		// Go has no standard video frame extraction; use a placeholder thumbnail.
		thumbData := thumb.Placeholder()
		result, err := b.cdnUploadWithThumb(ctx, sessionGeneration, content.Video, thumbData, userID, int(MediaVideo))
		if err != nil {
			return err
		}
		videoItem := &VideoItem{
			Media:     &result.Media,
			VideoSize: int64(result.EncryptedFileSize),
		}
		if result.ThumbMedia.EncryptQueryParam != "" {
			videoItem.ThumbMedia = &result.ThumbMedia
		}
		_, err = b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type:      ItemVideo,
			VideoItem: videoItem,
		}})
		return err
	}

	// File (auto-route by extension)
	if content.File != nil {
		fileName := content.FileName
		if fileName == "" {
			fileName = "file.bin"
		}
		cat := categorizeByExtension(fileName)
		if cat == "image" {
			return b.sendContent(ctx, sessionGeneration, userID, contextToken, SendContent{Image: content.File})
		}
		if cat == "video" {
			return b.sendContent(ctx, sessionGeneration, userID, contextToken, SendContent{Video: content.File})
		}
		// Generic file
		result, err := b.cdnUpload(ctx, sessionGeneration, content.File, userID, int(MediaFile))
		if err != nil {
			return err
		}
		_, err = b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type: ItemFile,
			FileItem: &FileItem{
				Media:    &result.Media,
				FileName: fileName,
				Len:      strconv.Itoa(len(content.File)),
			},
		}})
		return err
	}

	// Caption-only is valid: we already sent it above.
	if content.Caption != "" {
		return nil
	}

	return fmt.Errorf("empty SendContent")
}

func (b *Bot) cdnDownload(ctx context.Context, media *CDNMedia, aeskeyOverride string) ([]byte, error) {
	if media == nil {
		return nil, fmt.Errorf("missing CDN media")
	}
	downloadURL := media.FullURL
	if downloadURL == "" {
		if media.EncryptQueryParam == "" {
			return nil, fmt.Errorf("missing CDN encrypted_query_param")
		}
		downloadURL = fmt.Sprintf("%s/download?encrypted_query_param=%s",
			protocol.CDNBaseURL, url.QueryEscape(media.EncryptQueryParam))
	}

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdn download request: %w", err)
	}
	resp, err := b.client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cdn download failed: HTTP %d", resp.StatusCode)
	}

	reader := io.LimitReader(resp.Body, maxDownloadBytes+1)
	ciphertext, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("cdn download read: %w", err)
	}
	if len(ciphertext) > maxDownloadBytes {
		return nil, fmt.Errorf("cdn download exceeds %d bytes", maxDownloadBytes)
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

func (b *Bot) cdnUpload(ctx context.Context, sessionGeneration uint64, data []byte, userID string, mediaType int) (*UploadResult, error) {
	return b.cdnUploadWithThumb(ctx, sessionGeneration, data, nil, userID, mediaType)
}

func (b *Bot) cdnUploadWithThumb(ctx context.Context, sessionGeneration uint64, data, thumbData []byte, userID string, mediaType int) (*UploadResult, error) {
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

	thumbReq := protocol.GetUploadURLRequest{
		FileKey:     fileKey,
		MediaType:   mediaType,
		ToUserID:    userID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5Hex,
		FileSize:    len(ciphertext),
		NoNeedThumb: thumbData == nil,
		AESKey:      crypto.EncodeAESKeyHex(aesKey),
	}
	var thumbAESKey []byte
	if thumbData != nil {
		thumbAESKey, err = crypto.GenerateAESKey()
		if err != nil {
			return nil, fmt.Errorf("generate thumb aes key: %w", err)
		}
		thumbCipher, err := crypto.EncryptAESECB(thumbData, thumbAESKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt thumb: %w", err)
		}
		thumbMD5 := md5.Sum(thumbData)
		thumbReq.ThumbRawSize = len(thumbData)
		thumbReq.ThumbFileMD5 = hex.EncodeToString(thumbMD5[:])
		thumbReq.ThumbFileSize = len(thumbCipher)
	}
	uploadResp, requestToken, err := authenticatedRequestResult(b, ctx, sessionGeneration, func(requestContext context.Context, baseURL, token string) (*protocol.GetUploadURLResponse, error) {
		return b.client.GetUploadURL(requestContext, baseURL, token, thumbReq)
	})
	if requestToken == "" {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", b.handleAuthenticatedErrorForSession(sessionGeneration, requestToken, err))
	}
	uploadURL := uploadResp.UploadFullURL
	if uploadURL == "" {
		if uploadResp.UploadParam == "" {
			return nil, fmt.Errorf("getuploadurl did not return upload_param")
		}
		uploadURL = protocol.BuildCDNUploadURL(protocol.CDNBaseURL, uploadResp.UploadParam, fileKey)
	}

	encryptQueryParam, err := b.client.UploadToCDN(ctx, uploadURL, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}

	result := &UploadResult{
		Media: CDNMedia{
			EncryptQueryParam: encryptQueryParam,
			AESKey:            crypto.EncodeAESKeyBase64(aesKey),
			EncryptType:       1,
		},
		AESKey:            aesKey,
		EncryptedFileSize: len(ciphertext),
	}

	if thumbData != nil && uploadResp.ThumbUploadParam != "" {
		thumbCipher, err := crypto.EncryptAESECB(thumbData, thumbAESKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt thumb for upload: %w", err)
		}
		thumbURL := protocol.BuildCDNUploadURL(protocol.CDNBaseURL, uploadResp.ThumbUploadParam, fileKey+"_thumb")
		thumbParam, err := b.client.UploadToCDN(ctx, thumbURL, thumbCipher)
		if err != nil {
			return nil, fmt.Errorf("thumb upload: %w", err)
		}
		if thumbParam != "" {
			result.ThumbMedia = CDNMedia{
				EncryptQueryParam: thumbParam,
				AESKey:            crypto.EncodeAESKeyBase64(thumbAESKey),
				EncryptType:       1,
			}
		}
	}
	if _, err := b.readySession(sessionGeneration); err != nil {
		return nil, err
	}

	return result, nil
}

const (
	maxDownloadBytes = 100 * 1024 * 1024
	maxTextChars     = 2000
)

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

func (b *Bot) sendText(ctx context.Context, sessionGeneration uint64, userID, text, contextToken string) error {
	if err := requireContextToken(userID, contextToken); err != nil {
		return err
	}
	if b.opts.StripMarkdown {
		text = markdown.StripMarkdown(text)
	}
	chunks := chunkText(text, maxTextChars)
	for _, chunk := range chunks {
		_, err := b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type:     ItemText,
			TextItem: &TextItem{Text: chunk},
		}})
		if err != nil {
			return err
		}
	}
	return nil
}

// notifyError sends a short error notice to the user when NotifyErrors is enabled.
// Errors are best-effort; failures to send the notice are logged but not returned.
func (b *Bot) notifyError(ctx context.Context, sessionGeneration uint64, userID, contextToken string, err error) {
	if !b.opts.NotifyErrors {
		return
	}
	if requireContextToken(userID, contextToken) != nil {
		return
	}
	if _, readyErr := b.readySession(sessionGeneration); readyErr != nil {
		return
	}
	msg := "⚠️ 消息发送失败，请稍后重试。"
	_, e := b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
		Type:     ItemText,
		TextItem: &TextItem{Text: msg},
	}})
	if e != nil {
		b.log("warn", "failed to send error notice: %v", e)
	}
}

func (b *Bot) rememberContext(wire *WireMessage) error {
	b.mu.Lock()
	sessionGeneration := b.sessionGeneration
	b.mu.Unlock()
	return b.rememberContextForSession(sessionGeneration, wire)
}

func (b *Bot) rememberContextForSession(sessionGeneration uint64, wire *WireMessage) error {
	userID := peerUserID(wire)
	if userID != "" && wire.ContextToken != "" {
		if err := b.persistContextToken(sessionGeneration, userID, wire.ContextToken); err != nil {
			return fmt.Errorf("persist context token: %w", err)
		}
	}
	return nil
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
		SessionID:    wire.SessionID,
		GroupID:      wire.GroupID,
		RunID:        wire.RunID,
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
			if item.RefMsg.MessageItem != nil {
				q.Type = detectType([]MessageItem{*item.RefMsg.MessageItem})
				if item.RefMsg.MessageItem.TextItem != nil {
					q.Text = item.RefMsg.MessageItem.TextItem.Text
				}
			}
			msg.QuotedMessage = q
		}
	}

	return msg
}

func (b *Bot) configuredHandler() (MessageHandler, error) {
	b.mu.Lock()
	handler := b.handler
	middlewares := append([]Middleware(nil), b.middlewares...)
	b.mu.Unlock()
	return composeHandler(handler, middlewares)
}

func composeHandler(handler MessageHandler, middlewares []Middleware) (configured MessageHandler, err error) {
	if handler == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			configured = nil
			err = fmt.Errorf("configure message middleware: %v", recovered)
		}
	}()
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] == nil {
			continue
		}
		handler = middlewares[i](handler)
		if handler == nil {
			return nil, fmt.Errorf("configure message middleware %d: returned nil handler", i)
		}
	}
	return handler, nil
}

func (b *Bot) invokeHandler(ctx context.Context, handler MessageHandler, msg *IncomingMessage) (result MessageResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			b.log("error", "Message handler panic: %v\n%s", recovered, debug.Stack())
			result = RetryMessage(fmt.Errorf("message handler panic: %v", recovered))
		}
	}()
	return handler.HandleMessage(ctx, msg)
}

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

func detectType(items []MessageItem) ContentType {
	for _, item := range items {
		switch item.Type {
		case ItemImage:
			return ContentImage
		case ItemVoice:
			return ContentVoice
		case ItemFile:
			return ContentFile
		case ItemVideo:
			return ContentVideo
		case ItemToolCallStart:
			return ContentToolCallStart
		case ItemToolCallResult:
			return ContentToolCallResult
		}
	}
	return ContentText
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
	if limit <= 0 {
		return []string{text}
	}
	if runeLen(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if runeLen(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		prefix := firstRunes(text, limit)
		cut := len(prefix)
		if idx := strings.LastIndex(prefix, "\n\n"); idx >= 0 && runeLen(prefix[:idx]) > limit*3/10 {
			cut = idx + 2
		} else if idx := strings.LastIndex(prefix, "\n"); idx >= 0 && runeLen(prefix[:idx]) > limit*3/10 {
			cut = idx + 1
		} else if idx := strings.LastIndex(prefix, " "); idx >= 0 && runeLen(prefix[:idx]) > limit*3/10 {
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

func firstRunes(text string, limit int) string {
	count := 0
	for idx := range text {
		if count == limit {
			return text[:idx]
		}
		count++
	}
	return text
}

func runeLen(text string) int {
	count := 0
	for range text {
		count++
	}
	return count
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
