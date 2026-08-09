package wechatbot

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

// OutboundMessage is one item sent by one /sendmessage request.
// Set ClientID to reuse the same identity for an explicit retry.
type OutboundMessage struct {
	ClientID string
	RunID    string
	Item     MessageItem
}

// SendResult identifies the outbound request. It is returned even when the
// transport fails so callers can retry with the same ClientID.
type SendResult struct {
	ClientID string
	RunID    string
}

// SendRequest is passed to BeforeSend after ClientID generation and before the
// message is encoded. UserID and ClientID identify the validated destination
// and request and must not be changed by hooks.
type SendRequest struct {
	UserID  string
	Message OutboundMessage
}

// SendOutcome is passed to AfterSend after an HTTP send attempt.
type SendOutcome struct {
	Request SendRequest
	Result  SendResult
	Err     error
}

// ToolCallStatus is the terminal state reported for a tool call.
type ToolCallStatus string

const (
	ToolCallCompleted ToolCallStatus = "completed"
	ToolCallFailed    ToolCallStatus = "failed"
	ToolCallBlocked   ToolCallStatus = "blocked"
	ToolCallUnknown   ToolCallStatus = "unknown"
)

// NewToolCallStartMessage creates a tool-call progress message.
func NewToolCallStartMessage(runID, toolName, toolCallID string) OutboundMessage {
	completed := false
	return OutboundMessage{
		RunID: runID,
		Item: MessageItem{
			Type:         ItemToolCallStart,
			CreateTimeMs: time.Now().UnixMilli(),
			IsCompleted:  &completed,
			ToolCallStartItem: &ToolCallStartItem{
				ToolName:   toolName,
				ToolCallID: toolCallID,
			},
		},
	}
}

// NewToolCallResultMessage creates a terminal tool-call progress message.
func NewToolCallResultMessage(runID, toolName, toolCallID string, status ToolCallStatus) OutboundMessage {
	completed := true
	status = normalizeToolCallStatus(status)
	return OutboundMessage{
		RunID: runID,
		Item: MessageItem{
			Type:         ItemToolCallResult,
			CreateTimeMs: time.Now().UnixMilli(),
			IsCompleted:  &completed,
			ToolCallResultItem: &ToolCallResultItem{
				ToolName:   toolName,
				ToolCallID: toolCallID,
				Status:     string(status),
			},
		},
	}
}

func normalizeToolCallStatus(status ToolCallStatus) ToolCallStatus {
	switch status {
	case ToolCallCompleted, ToolCallFailed, ToolCallBlocked:
		return status
	default:
		return ToolCallUnknown
	}
}

// SendMessage sends exactly one message item to a user.
func (b *Bot) SendMessage(ctx context.Context, userID string, msg OutboundMessage) (SendResult, error) {
	contextToken := b.contextTokens.Get(userID)
	result, err := b.sendMessage(ctx, userID, contextToken, msg)
	if err != nil {
		b.notifyError(ctx, userID, contextToken, err)
	}
	return result, err
}

// ReplyMessage replies with exactly one message item using an inbound message's
// context token.
func (b *Bot) ReplyMessage(ctx context.Context, inbound *IncomingMessage, msg OutboundMessage) (SendResult, error) {
	if inbound == nil {
		return SendResult{}, fmt.Errorf("inbound message is nil")
	}
	if err := requireContextToken(inbound.UserID, inbound.ContextToken); err != nil {
		return SendResult{}, err
	}
	if err := b.contextTokens.Set(inbound.UserID, inbound.ContextToken); err != nil {
		b.log("warn", "failed to persist context token: %v", err)
	}
	result, err := b.sendMessage(ctx, inbound.UserID, inbound.ContextToken, msg)
	if err != nil {
		b.notifyError(ctx, inbound.UserID, inbound.ContextToken, err)
	}
	return result, err
}

func (b *Bot) sendMessage(ctx context.Context, userID, contextToken string, msg OutboundMessage) (SendResult, error) {
	if err := requireContextToken(userID, contextToken); err != nil {
		return SendResult{}, err
	}
	creds, err := b.readyCreds()
	if err != nil {
		return SendResult{}, err
	}
	if msg.ClientID == "" {
		msg.ClientID, err = newClientID()
		if err != nil {
			return SendResult{}, err
		}
	}

	validatedUserID := userID
	validatedClientID := msg.ClientID
	request := SendRequest{UserID: userID, Message: msg}
	result := SendResult{ClientID: msg.ClientID, RunID: msg.RunID}
	if err := b.hooks.BeforeSend.Run(&request); err != nil {
		return result, fmt.Errorf("BeforeSend hook failed: %w", err)
	}
	if request.UserID != validatedUserID {
		return result, fmt.Errorf("BeforeSend hook cannot change user ID")
	}
	if request.Message.ClientID != validatedClientID {
		return result, fmt.Errorf("BeforeSend hook cannot change client ID")
	}
	if err := validateOutboundMessage(request.Message); err != nil {
		return result, err
	}

	result.RunID = request.Message.RunID
	wire := protocol.BuildMessage(
		request.UserID,
		contextToken,
		request.Message.ClientID,
		request.Message.RunID,
		request.Message.Item,
	)
	sendErr := b.client.SendMessage(ctx, creds.BaseURL, creds.Token, wire)
	outcome := SendOutcome{Request: request, Result: result, Err: sendErr}
	if hookErr := b.hooks.AfterSend.Run(outcome); hookErr != nil {
		b.reportError(fmt.Errorf("AfterSend hook failed: %w", hookErr))
	}
	return result, sendErr
}

func validateOutboundMessage(msg OutboundMessage) error {
	if msg.ClientID == "" {
		return fmt.Errorf("outbound message client ID is empty")
	}
	switch msg.Item.Type {
	case ItemText:
		if msg.Item.TextItem == nil {
			return fmt.Errorf("text message is missing text_item")
		}
	case ItemImage:
		if msg.Item.ImageItem == nil {
			return fmt.Errorf("image message is missing image_item")
		}
	case ItemVoice:
		if msg.Item.VoiceItem == nil {
			return fmt.Errorf("voice message is missing voice_item")
		}
	case ItemFile:
		if msg.Item.FileItem == nil {
			return fmt.Errorf("file message is missing file_item")
		}
	case ItemVideo:
		if msg.Item.VideoItem == nil {
			return fmt.Errorf("video message is missing video_item")
		}
	case ItemToolCallStart:
		if msg.Item.ToolCallStartItem == nil {
			return fmt.Errorf("tool-call start message is missing tool_call_start_item")
		}
	case ItemToolCallResult:
		if msg.Item.ToolCallResultItem == nil {
			return fmt.Errorf("tool-call result message is missing tool_call_result_item")
		}
	default:
		return fmt.Errorf("unsupported outbound message item type %d", msg.Item.Type)
	}
	return nil
}

func newClientID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate client ID: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
