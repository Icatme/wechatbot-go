package wechatbot

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/protocol"
	"github.com/Icatme/wechatbot-go/internal/store"
)

var errExplicitReauthentication = errors.New("explicit reauthentication requested")
var errDurableReauthentication = errors.New("durable reauthentication marker found")

type reauthState struct {
	done               chan struct{}
	invalidToken       string
	invalidTokenSHA256 string
	accountID          string
	contextPaths       []string
	generation         uint64
	err                error
}

// ReauthRequired reports whether this Bot has observed that the current
// credentials were invalidated and explicit reauthentication is required.
func (b *Bot) ReauthRequired() bool {
	b.mu.Lock()
	required := b.reauth != nil
	reauthStore := b.reauthStore
	b.mu.Unlock()
	return required || (reauthStore != nil && reauthStore.Required())
}

func (b *Bot) handleAuthenticatedError(err error) error {
	b.mu.Lock()
	generation := b.sessionGeneration
	token := ""
	if b.creds != nil {
		token = b.creds.Token
	}
	b.mu.Unlock()
	return b.handleAuthenticatedErrorForSession(generation, token, err)
}

func (b *Bot) handleAuthenticatedErrorForSession(generation uint64, token string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *protocol.APIError
	if errors.As(err, &apiErr) && apiErr.IsSessionExpired() {
		return b.requireReauthenticationForSession(generation, token, err)
	}
	if stateErr := b.sessionError(generation); stateErr != nil {
		return errors.Join(err, stateErr)
	}
	return err
}

func (b *Bot) requireReauthentication(cause error) error {
	b.mu.Lock()
	generation := b.sessionGeneration
	token := ""
	if b.creds != nil {
		token = b.creds.Token
	}
	b.mu.Unlock()
	return b.requireReauthenticationForSession(generation, token, cause)
}

func (b *Bot) requireReauthenticationForSession(generation uint64, token string, cause error) error {
	b.sessionMu.Lock()
	b.mu.Lock()
	if b.sessionGeneration != generation {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return errors.Join(cause, ErrSessionChanged)
	}
	if state := b.reauth; state != nil {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		if state.generation != generation {
			return errors.Join(cause, ErrSessionChanged)
		}
		return waitReauthState(state)
	}
	if b.creds == nil || b.creds.Token != token {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return errors.Join(cause, ErrSessionChanged)
	}

	state := &reauthState{
		done:               make(chan struct{}),
		invalidToken:       b.creds.Token,
		invalidTokenSHA256: tokenSHA256(b.creds.Token),
		accountID:          b.creds.AccountID,
		contextPaths:       canonicalContextPaths(b.contextTokens.Path()),
		generation:         generation,
	}
	markerErr := b.markReauthentication(state)
	b.reauth = state
	b.creds = nil
	b.configCache = nil
	cancelSession := b.cancelSession
	b.sessionContext = nil
	b.cancelSession = nil
	cancel := b.cancelPoll
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cancelSession != nil {
		cancelSession()
	}
	cleanupErr := errors.Join(markerErr, b.cleanupReauthenticationFiles(state, b.contextTokens))
	b.sessionMu.Unlock()
	state.err = &ReauthRequiredError{Cause: cause, CleanupErr: cleanupErr}
	close(state.done)
	return state.err
}

func (b *Bot) prepareReauthentication() (*reauthState, error) {
	b.mu.Lock()
	state := b.reauth
	creds := b.creds
	generation := b.sessionGeneration
	b.mu.Unlock()

	created := false
	if state == nil {
		created = true
		if creds != nil {
			_ = b.requireReauthenticationForSession(generation, creds.Token, errExplicitReauthentication)
		} else {
			_ = b.requireReauthenticationWithoutCredentials(errExplicitReauthentication)
		}
		b.mu.Lock()
		state = b.reauth
		b.mu.Unlock()
	}
	if state == nil {
		return nil, nil
	}

	stateErr := waitReauthState(state)
	var reauthErr *ReauthRequiredError
	if !errors.As(stateErr, &reauthErr) || reauthErr.CleanupErr == nil {
		return state, nil
	}
	if created {
		return nil, stateErr
	}
	return b.retryReauthenticationCleanup(state, reauthErr.Cause)
}

func (b *Bot) retryReauthenticationCleanup(previous *reauthState, cause error) (*reauthState, error) {
	next := &reauthState{
		done:               make(chan struct{}),
		invalidToken:       previous.invalidToken,
		invalidTokenSHA256: previous.invalidTokenSHA256,
		accountID:          previous.accountID,
		contextPaths:       append([]string(nil), previous.contextPaths...),
		generation:         previous.generation,
	}

	b.sessionMu.Lock()
	stored, _ := auth.LoadCredentials(b.opts.CredPath)
	if stored != nil {
		if next.invalidTokenSHA256 == "" {
			next.invalidToken = stored.Token
			next.invalidTokenSHA256 = tokenSHA256(stored.Token)
		}
		if next.accountID == "" {
			next.accountID = stored.AccountID
		}
		if stored.AccountID != "" {
			next.contextPaths = canonicalContextPaths(append(next.contextPaths, store.NewContextStore(stored.AccountID, "").Path())...)
		}
	}
	b.mu.Lock()
	if b.reauth != previous {
		current := b.reauth
		b.mu.Unlock()
		b.sessionMu.Unlock()
		if current == nil {
			return nil, nil
		}
		return current, waitReauthState(current)
	}
	markerErr := b.markReauthentication(next, b.contextTokens.Path())
	b.reauth = next
	contextTokens := b.contextTokens
	b.mu.Unlock()

	cleanupErr := errors.Join(markerErr, b.cleanupReauthenticationFiles(next, contextTokens))
	b.sessionMu.Unlock()
	next.err = &ReauthRequiredError{Cause: cause, CleanupErr: cleanupErr}
	close(next.done)
	if cleanupErr != nil {
		return nil, next.err
	}
	return next, nil
}

func (b *Bot) requireReauthenticationWithoutCredentials(cause error) error {
	b.sessionMu.Lock()
	stored, _ := auth.LoadCredentials(b.opts.CredPath)
	b.mu.Lock()
	if state := b.reauth; state != nil {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return waitReauthState(state)
	}
	accountID := b.opts.AccountID
	invalidToken := ""
	contextPaths := []string{b.contextTokens.Path()}
	if stored != nil {
		invalidToken = stored.Token
		if accountID == "" {
			accountID = stored.AccountID
		}
		if stored.AccountID != "" {
			contextPaths = append(contextPaths, store.NewContextStore(stored.AccountID, "").Path())
		}
	}
	state := &reauthState{
		done:               make(chan struct{}),
		invalidToken:       invalidToken,
		invalidTokenSHA256: tokenSHA256(invalidToken),
		accountID:          accountID,
		contextPaths:       canonicalContextPaths(contextPaths...),
		generation:         b.sessionGeneration,
	}
	markerErr := b.markReauthentication(state)
	b.reauth = state
	b.creds = nil
	b.configCache = nil
	cancelSession := b.cancelSession
	b.sessionContext = nil
	b.cancelSession = nil
	cancel := b.cancelPoll
	contextTokens := b.contextTokens
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cancelSession != nil {
		cancelSession()
	}
	cleanupErr := errors.Join(markerErr, b.cleanupReauthenticationFiles(state, contextTokens))
	b.sessionMu.Unlock()
	state.err = &ReauthRequiredError{Cause: cause, CleanupErr: cleanupErr}
	close(state.done)
	return state.err
}

func (b *Bot) restoreDurableReauthentication() error {
	b.mu.Lock()
	if b.reauth != nil {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	record, err := b.reauthStore.Load()
	if err != nil {
		_ = b.requireReauthenticationWithoutCredentials(errors.Join(errDurableReauthentication, err))
		return nil
	}
	if record == nil {
		return nil
	}

	b.sessionMu.Lock()
	b.mu.Lock()
	if b.reauth != nil {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return nil
	}
	currentContextPath := b.contextTokens.Path()
	contextTokens := b.contextTokens
	cursorStore := b.cursorStore
	replayStore := b.replayStore
	if b.opts.AccountID == "" && record.AccountID != "" {
		if b.opts.ContextTokenPath == "" {
			contextTokens = store.NewContextStore(record.AccountID, "")
		}
		if b.opts.CursorPath == "" {
			cursorStore = store.NewCursorStore(record.AccountID, "")
		}
		if b.opts.ReplayPath == "" {
			replayStore = store.NewReplayStore(record.AccountID, "", store.DefaultReplayTTL)
		}
	}
	if b.opts.AccountID != "" && record.AccountID != "" && b.opts.AccountID != record.AccountID {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return &ReauthRequiredError{
			Cause:      errDurableReauthentication,
			CleanupErr: fmt.Errorf("reauthentication marker account %q does not match configured account %q", record.AccountID, b.opts.AccountID),
		}
	}
	if err := b.validateStatePaths(contextTokens, cursorStore, replayStore); err != nil {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return &ReauthRequiredError{Cause: errDurableReauthentication, CleanupErr: err}
	}
	expectedAccountID := record.AccountID
	if expectedAccountID == "" {
		expectedAccountID = b.opts.AccountID
	}
	state := &reauthState{
		done:               make(chan struct{}),
		invalidTokenSHA256: record.InvalidTokenSHA256,
		accountID:          expectedAccountID,
		contextPaths:       canonicalContextPaths(append(record.ContextPaths, currentContextPath, contextTokens.Path())...),
		generation:         b.sessionGeneration,
	}
	if err := b.validateReauthenticationContextPaths(state.contextPaths, cursorStore, replayStore); err != nil {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return &ReauthRequiredError{Cause: errDurableReauthentication, CleanupErr: err}
	}
	if err := b.markReauthentication(state); err != nil {
		b.mu.Unlock()
		b.sessionMu.Unlock()
		return &ReauthRequiredError{Cause: errDurableReauthentication, CleanupErr: err}
	}
	if b.opts.AccountID == "" && record.AccountID != "" {
		b.opts.AccountID = record.AccountID
		b.contextTokens = contextTokens
		b.cursorStore = cursorStore
		b.replayStore = replayStore
	}
	b.reauth = state
	b.creds = nil
	b.configCache = nil
	cancelSession := b.cancelSession
	b.sessionContext = nil
	b.cancelSession = nil
	cancel := b.cancelPoll
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cancelSession != nil {
		cancelSession()
	}
	cleanupErr := b.cleanupReauthenticationFiles(state, contextTokens)
	b.sessionMu.Unlock()
	state.err = &ReauthRequiredError{Cause: errDurableReauthentication, CleanupErr: cleanupErr}
	close(state.done)
	return nil
}

func (b *Bot) markReauthentication(state *reauthState, contextPaths ...string) error {
	state.contextPaths = canonicalContextPaths(append(state.contextPaths, contextPaths...)...)
	if err := b.validateReauthenticationContextPaths(state.contextPaths, b.cursorStore, b.replayStore); err != nil {
		return err
	}
	if err := b.reauthStore.Mark(store.ReauthRecord{
		InvalidTokenSHA256: state.invalidTokenSHA256,
		AccountID:          state.accountID,
		ContextPaths:       state.contextPaths,
	}); err != nil {
		return fmt.Errorf("persist reauthentication requirement: %w", err)
	}
	return nil
}

func (b *Bot) validateCurrentStatePaths() error {
	b.mu.Lock()
	contextTokens := b.contextTokens
	cursorStore := b.cursorStore
	replayStore := b.replayStore
	b.mu.Unlock()
	return b.validateStatePaths(contextTokens, cursorStore, replayStore)
}

func (b *Bot) validateStatePaths(contextTokens *store.ContextStore, cursorStore *store.CursorStore, replayStore *store.ReplayStore) error {
	paths := []struct {
		name string
		path string
	}{
		{name: "credentials", path: b.credentialsPath()},
		{name: "reauthentication marker", path: b.reauthStore.Path()},
		{name: "context tokens", path: contextTokens.Path()},
		{name: "cursor", path: cursorStore.Path()},
		{name: "replay deduplication", path: replayStore.Path()},
	}
	seen := make(map[string]string, len(paths))
	for _, entry := range paths {
		path := canonicalStatePath(entry.path)
		key := statePathKey(path)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("state paths must be distinct: %s and %s both resolve to %q", previous, entry.name, path)
		}
		seen[key] = entry.name
	}
	return nil
}

func (b *Bot) validateReauthenticationContextPaths(paths []string, cursorStore *store.CursorStore, replayStore *store.ReplayStore) error {
	protected := map[string]string{
		statePathKey(b.credentialsPath()):  "credentials",
		statePathKey(b.reauthStore.Path()): "reauthentication marker",
		statePathKey(cursorStore.Path()):   "cursor",
		statePathKey(replayStore.Path()):   "replay deduplication",
	}
	for _, path := range canonicalContextPaths(paths...) {
		if name, ok := protected[statePathKey(path)]; ok {
			return fmt.Errorf("context state path %q collides with %s state", path, name)
		}
	}
	return nil
}

func (b *Bot) credentialsPath() string {
	if b.opts.CredPath != "" {
		return b.opts.CredPath
	}
	return auth.DefaultCredPath()
}

func (b *Bot) cleanupReauthenticationFiles(state *reauthState, primary *store.ContextStore) error {
	return errors.Join(
		clearContextPaths(primary, state.contextPaths),
		auth.ClearCredentials(b.opts.CredPath),
	)
}

func (b *Bot) persistAndClearReauthenticationContexts(state *reauthState, primary *store.ContextStore) error {
	if state == nil {
		return primary.Clear()
	}
	state.contextPaths = canonicalContextPaths(append(state.contextPaths, primary.Path())...)
	if err := b.reauthStore.Mark(store.ReauthRecord{
		InvalidTokenSHA256: state.invalidTokenSHA256,
		AccountID:          state.accountID,
		ContextPaths:       state.contextPaths,
	}); err != nil {
		return fmt.Errorf("update reauthentication requirement: %w", err)
	}
	return clearContextPaths(primary, state.contextPaths)
}

func clearContextPaths(primary *store.ContextStore, paths []string) error {
	primaryPath := statePathKey(primary.Path())
	var clearErrors []error
	for _, path := range canonicalContextPaths(paths...) {
		var err error
		if statePathKey(path) == primaryPath {
			err = primary.Clear()
		} else {
			err = store.NewContextStore("", path).Clear()
		}
		if err != nil {
			clearErrors = append(clearErrors, fmt.Errorf("clear context state %q: %w", path, err))
		}
	}
	return errors.Join(clearErrors...)
}

func canonicalContextPaths(paths ...string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = canonicalStatePath(path)
		key := statePathKey(path)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, path)
	}
	return result
}

func canonicalStatePath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

func statePathKey(path string) string {
	path = canonicalStatePath(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

func tokenSHA256(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:])
}

func (s *reauthState) rejectsToken(token string) bool {
	if s == nil || token == "" {
		return false
	}
	if s.invalidToken != "" && token == s.invalidToken {
		return true
	}
	return s.invalidTokenSHA256 != "" && tokenSHA256(token) == s.invalidTokenSHA256
}

func (b *Bot) reauthError() error {
	b.mu.Lock()
	state := b.reauth
	b.mu.Unlock()
	if state == nil {
		return nil
	}
	return waitReauthState(state)
}

func (b *Bot) sessionError(generation uint64) error {
	b.mu.Lock()
	currentGeneration := b.sessionGeneration
	state := b.reauth
	creds := b.creds
	b.mu.Unlock()
	if currentGeneration != generation {
		return ErrSessionChanged
	}
	if state != nil {
		return waitReauthState(state)
	}
	if creds == nil {
		return ErrSessionChanged
	}
	return nil
}

func (b *Bot) normalizeSessionChangeError(generation uint64, err error) error {
	if !errors.Is(b.sessionError(generation), ErrSessionChanged) {
		return err
	}
	retained := make([]error, 0, 3)
	collectSessionChangeErrors(err, &retained)
	retained = append(retained, ErrSessionChanged)
	return errors.Join(retained...)
}

func collectSessionChangeErrors(err error, retained *[]error) {
	if err == nil || err == ErrSessionChanged || err == ErrReauthRequired {
		return
	}
	if reauthErr, ok := err.(*ReauthRequiredError); ok {
		collectSessionChangeErrors(reauthErr.Cause, retained)
		collectSessionChangeErrors(reauthErr.CleanupErr, retained)
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			collectSessionChangeErrors(child, retained)
		}
		return
	}
	if errors.Is(err, ErrReauthRequired) || errors.Is(err, ErrSessionChanged) {
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			collectSessionChangeErrors(wrapped.Unwrap(), retained)
		}
		return
	}
	*retained = append(*retained, err)
}

func waitReauthState(state *reauthState) error {
	if state == nil {
		return nil
	}
	<-state.done
	if state.err != nil {
		return state.err
	}
	return fmt.Errorf("%w", ErrReauthRequired)
}

func (b *Bot) persistContextToken(sessionGeneration uint64, userID, token string) error {
	if userID == "" || token == "" {
		return nil
	}
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()

	b.mu.Lock()
	state := b.reauth
	currentGeneration := b.sessionGeneration
	contextTokens := b.contextTokens
	b.mu.Unlock()
	if currentGeneration != sessionGeneration {
		return ErrSessionChanged
	}
	if state != nil {
		return fmt.Errorf("%w", ErrReauthRequired)
	}
	return contextTokens.Set(userID, token)
}
