package wechatbot

import (
	"context"
	"fmt"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/config"
	"github.com/Icatme/wechatbot-go/internal/protocol"
)

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
