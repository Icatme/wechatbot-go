package wechatbot

import (
	"errors"
	"fmt"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

var (
	// ErrNotLoggedIn indicates that no usable credentials are installed.
	ErrNotLoggedIn = errors.New("wechatbot: not logged in")
	// ErrLoginInProgress indicates that a new authentication attempt could not
	// start because another attempt is active.
	ErrLoginInProgress = errors.New("wechatbot: login already in progress")
	// ErrSessionChanged indicates that an operation belongs to an older
	// authenticated session and was rejected without invalidating the current one.
	ErrSessionChanged = errors.New("wechatbot: authenticated session changed")
	// ErrReauthRequired indicates that credentials were invalidated and an
	// explicit Reauthenticate call is required.
	ErrReauthRequired = errors.New("wechatbot: reauthentication required")
)

// APIError describes a non-success iLink API response while preserving the
// endpoint, HTTP status, ret, and errcode dimensions.
type APIError = protocol.APIError

// ReauthRequiredError preserves the API error that invalidated the session and
// any local cleanup failure. It always matches ErrReauthRequired.
type ReauthRequiredError struct {
	Cause      error
	CleanupErr error
}

func (e *ReauthRequiredError) Error() string {
	if e.CleanupErr != nil {
		return fmt.Sprintf("%v: %v (cleanup: %v)", ErrReauthRequired, e.Cause, e.CleanupErr)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%v: %v", ErrReauthRequired, e.Cause)
	}
	return ErrReauthRequired.Error()
}

// Unwrap preserves the original API error for errors.As.
func (e *ReauthRequiredError) Unwrap() error { return e.Cause }

// Is makes every ReauthRequiredError match ErrReauthRequired.
func (e *ReauthRequiredError) Is(target error) bool { return target == ErrReauthRequired }
