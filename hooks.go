package wechatbot

import "sync"

// HookFunc is called at specific lifecycle points. Returning an error stops
// further processing of that hook chain.
type HookFunc[T any] func(payload T) error

// HookRegistry manages named hooks for bot lifecycle events.
type HookRegistry[T any] struct {
	mu    sync.RWMutex
	hooks []HookFunc[T]
}

// Register adds a hook to the registry.
func (r *HookRegistry[T]) Register(hook HookFunc[T]) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks = append(r.hooks, hook)
}

// Run executes all registered hooks in registration order.
func (r *HookRegistry[T]) Run(payload T) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	hooks := append([]HookFunc[T](nil), r.hooks...)
	r.mu.RUnlock()
	for _, h := range hooks {
		if h == nil {
			continue
		}
		if err := h(payload); err != nil {
			return err
		}
	}
	return nil
}

// LifecycleHooks group all available bot hooks.
type LifecycleHooks struct {
	// BeforeLogin runs after QR/login starts but before credentials are finalized.
	BeforeLogin HookRegistry[*Credentials]
	// AfterLogin runs after credentials are loaded or created.
	AfterLogin HookRegistry[*Credentials]
	// OnError runs when the bot encounters a non-fatal runtime error.
	OnError HookRegistry[error]
	// BeforeSend runs after ClientID generation and before a message is sent.
	BeforeSend HookRegistry[*SendRequest]
	// AfterSend observes the result of an attempted message send.
	AfterSend HookRegistry[SendOutcome]
	// AfterReceive runs after a message is parsed and before handlers run.
	AfterReceive HookRegistry[*IncomingMessage]
}
