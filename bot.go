package wechatbot

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/config"
	"github.com/Icatme/wechatbot-go/internal/protocol"
	"github.com/Icatme/wechatbot-go/internal/store"
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
	reauthStore        reauthPersistence
	running            bool
	mu                 sync.Mutex
	loginMu            sync.Mutex
	sessionMu          sync.Mutex
	requestMu          sync.RWMutex
	cancelPoll         context.CancelFunc
	hooks              LifecycleHooks
	logger             loggerAdapter
}

type reauthPersistence interface {
	Path() string
	Load() (*store.ReauthRecord, error)
	Mark(store.ReauthRecord) error
	Clear() error
	Required() bool
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
	reauthCredentialPath := o.CredPath
	if reauthCredentialPath == "" {
		reauthCredentialPath = auth.DefaultCredPath()
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
		reauthStore:   store.NewReauthStore(store.ReauthStatePath(reauthCredentialPath)),
		hooks:         LifecycleHooks{},
		logger:        logger,
	}
}
