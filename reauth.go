package wechatbot

import (
	"errors"
	"fmt"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/protocol"
)

var errExplicitReauthentication = errors.New("explicit reauthentication requested")

type reauthState struct {
	done         chan struct{}
	invalidToken string
	accountID    string
	generation   uint64
	err          error
}

// ReauthRequired reports whether the current credentials have been
// invalidated and explicit reauthentication is required.
func (b *Bot) ReauthRequired() bool {
	b.mu.Lock()
	required := b.reauth != nil
	b.mu.Unlock()
	return required
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
	b.mu.Lock()
	if b.sessionGeneration != generation {
		b.mu.Unlock()
		return errors.Join(cause, ErrSessionChanged)
	}
	if state := b.reauth; state != nil {
		b.mu.Unlock()
		if state.generation != generation {
			return errors.Join(cause, ErrSessionChanged)
		}
		return waitReauthState(state)
	}
	if b.creds == nil || b.creds.Token != token {
		b.mu.Unlock()
		return errors.Join(cause, ErrSessionChanged)
	}

	state := &reauthState{
		done:         make(chan struct{}),
		invalidToken: b.creds.Token,
		accountID:    b.creds.AccountID,
		generation:   generation,
	}
	b.reauth = state
	b.creds = nil
	b.configCache = nil
	cancelSession := b.cancelSession
	b.sessionContext = nil
	b.cancelSession = nil
	cancel := b.cancelPoll
	contextTokens := b.contextTokens
	credPath := b.opts.CredPath
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cancelSession != nil {
		cancelSession()
	}
	b.sessionMu.Lock()
	cleanupErr := errors.Join(
		auth.ClearCredentials(credPath),
		contextTokens.Clear(),
	)
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
	if state == nil && creds != nil {
		created = true
		_ = b.requireReauthenticationForSession(generation, creds.Token, errExplicitReauthentication)
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
		done:         make(chan struct{}),
		invalidToken: previous.invalidToken,
		accountID:    previous.accountID,
		generation:   previous.generation,
	}

	b.mu.Lock()
	if b.reauth != previous {
		current := b.reauth
		b.mu.Unlock()
		if current == nil {
			return nil, nil
		}
		return current, waitReauthState(current)
	}
	b.reauth = next
	contextTokens := b.contextTokens
	credPath := b.opts.CredPath
	b.mu.Unlock()

	b.sessionMu.Lock()
	cleanupErr := errors.Join(
		auth.ClearCredentials(credPath),
		contextTokens.Clear(),
	)
	b.sessionMu.Unlock()
	next.err = &ReauthRequiredError{Cause: cause, CleanupErr: cleanupErr}
	close(next.done)
	if cleanupErr != nil {
		return nil, next.err
	}
	return next, nil
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
