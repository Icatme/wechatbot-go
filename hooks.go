package wechatbot

// Middleware intercepts an incoming message and decides whether to pass it along.
// Return false to stop processing the message (the message is dropped).
type Middleware func(msg *IncomingMessage) bool

// HookFunc is called at specific lifecycle points. Returning an error stops
// further processing of that hook chain.
type HookFunc[T any] func(payload T) error

// HookRegistry manages named hooks for bot lifecycle events.
type HookRegistry[T any] struct {
	hooks []HookFunc[T]
}

// Register adds a hook to the registry.
func (r *HookRegistry[T]) Register(hook HookFunc[T]) {
	if r == nil {
		return
	}
	r.hooks = append(r.hooks, hook)
}

// Run executes all registered hooks in registration order.
func (r *HookRegistry[T]) Run(payload T) error {
	if r == nil {
		return nil
	}
	for _, h := range r.hooks {
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
	// BeforeSend runs before a message is sent; mutate payload to change content.
	BeforeSend HookRegistry[*SendContent]
	// AfterReceive runs after a message is parsed and before handlers run.
	AfterReceive HookRegistry[*IncomingMessage]
}
