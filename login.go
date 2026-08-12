package wechatbot

import (
	"context"
	"errors"
	"fmt"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/store"
)

// Login performs QR code login or loads stored credentials. A durable
// reauthentication marker makes a non-forced login fail with
// ErrReauthRequired without starting network authentication.
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
	if err := b.validateCurrentStatePaths(); err != nil {
		return nil, err
	}
	if err := b.restoreDurableReauthentication(); err != nil {
		return nil, err
	}
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
// accepting credentials that were invalidated by a -14 response. It is the
// only login path that can complete a durable reauthentication transition.
func (b *Bot) Reauthenticate(ctx context.Context) (*Credentials, error) {
	if !b.loginMu.TryLock() {
		return nil, ErrLoginInProgress
	}
	defer b.loginMu.Unlock()
	if err := b.validateCurrentStatePaths(); err != nil {
		return nil, err
	}
	if err := b.restoreDurableReauthentication(); err != nil {
		return nil, err
	}

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
	if reauth != nil && reauth.rejectsToken(creds.Token) {
		cleanupErr := auth.ClearCredentials(b.opts.CredPath)
		return nil, reauthenticationAttemptError(reauth, errors.Join(auth.ErrInvalidatedCredential, cleanupErr))
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
	if b.opts.ContextTokenPath == "" && (reauth == nil || b.opts.AccountID == "") {
		contextTokens = store.NewContextStore(accountID, "")
	}
	if b.opts.CursorPath == "" && (reauth == nil || b.opts.AccountID == "") {
		cursorStore = store.NewCursorStore(accountID, "")
	}
	if b.opts.ReplayPath == "" && (reauth == nil || b.opts.AccountID == "") {
		replayStore = store.NewReplayStore(accountID, "", store.DefaultReplayTTL)
	}
	if err := b.validateStatePaths(contextTokens, cursorStore, replayStore); err != nil {
		cleanupErr := auth.ClearCredentials(b.opts.CredPath)
		return nil, reauthenticationAttemptError(reauth, errors.Join(err, cleanupErr))
	}
	if reauth != nil {
		if err := b.validateReauthenticationContextPaths(reauth.contextPaths, cursorStore, replayStore); err != nil {
			cleanupErr := auth.ClearCredentials(b.opts.CredPath)
			return nil, reauthenticationAttemptError(reauth, errors.Join(err, cleanupErr))
		}
	}
	b.sessionMu.Lock()
	if force {
		if err := b.persistAndClearReauthenticationContexts(reauth, contextTokens); err != nil {
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
	if reauth != nil {
		if err := b.reauthStore.Clear(); err != nil {
			b.mu.Unlock()
			b.requestMu.Unlock()
			b.sessionMu.Unlock()
			cleanupErr := auth.ClearCredentials(b.opts.CredPath)
			return nil, reauthenticationAttemptError(reauth, errors.Join(fmt.Errorf("clear durable reauthentication marker: %w", err), cleanupErr))
		}
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
