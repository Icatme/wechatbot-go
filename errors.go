package wechatbot

import "github.com/Icatme/wechatbot-go/internal/protocol"

// APIError describes a non-success iLink API response while preserving the
// endpoint, HTTP status, ret, and errcode dimensions.
type APIError = protocol.APIError
