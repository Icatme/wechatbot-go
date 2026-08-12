package wechatbot

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (b *Bot) rememberContext(wire *WireMessage) error {
	b.mu.Lock()
	sessionGeneration := b.sessionGeneration
	b.mu.Unlock()
	return b.rememberContextForSession(sessionGeneration, wire)
}

func (b *Bot) rememberContextForSession(sessionGeneration uint64, wire *WireMessage) error {
	userID := peerUserID(wire)
	if userID != "" && wire.ContextToken != "" {
		if err := b.persistContextToken(sessionGeneration, userID, wire.ContextToken); err != nil {
			return fmt.Errorf("persist context token: %w", err)
		}
	}
	return nil
}

func (b *Bot) parseMessage(wire *WireMessage) *IncomingMessage {
	if wire.MessageType != MessageTypeUser {
		return nil
	}

	msg := &IncomingMessage{
		UserID:       wire.FromUserID,
		Text:         extractText(wire.ItemList),
		Type:         detectType(wire.ItemList),
		Timestamp:    time.UnixMilli(wire.CreateTimeMs),
		SessionID:    wire.SessionID,
		GroupID:      wire.GroupID,
		RunID:        wire.RunID,
		Raw:          wire,
		ContextToken: wire.ContextToken,
	}

	for _, item := range wire.ItemList {
		if item.ImageItem != nil {
			msg.Images = append(msg.Images, ImageContent{
				Media: item.ImageItem.Media, ThumbMedia: item.ImageItem.ThumbMedia,
				AESKey: item.ImageItem.AESKey, URL: item.ImageItem.URL,
				Width: item.ImageItem.ThumbWidth, Height: item.ImageItem.ThumbHeight,
			})
		}
		if item.VoiceItem != nil {
			msg.Voices = append(msg.Voices, VoiceContent{
				Media: item.VoiceItem.Media, Text: item.VoiceItem.Text,
				DurationMs: item.VoiceItem.Playtime, EncodeType: item.VoiceItem.EncodeType,
			})
		}
		if item.FileItem != nil {
			size, _ := strconv.ParseInt(item.FileItem.Len, 10, 64)
			msg.Files = append(msg.Files, FileContent{
				Media: item.FileItem.Media, FileName: item.FileItem.FileName,
				MD5: item.FileItem.MD5, Size: size,
			})
		}
		if item.VideoItem != nil {
			msg.Videos = append(msg.Videos, VideoContent{
				Media: item.VideoItem.Media, ThumbMedia: item.VideoItem.ThumbMedia,
				DurationMs: item.VideoItem.PlayLength,
			})
		}
		if item.RefMsg != nil {
			q := &QuotedMessage{Title: item.RefMsg.Title}
			if item.RefMsg.MessageItem != nil {
				q.Type = detectType([]MessageItem{*item.RefMsg.MessageItem})
				if item.RefMsg.MessageItem.TextItem != nil {
					q.Text = item.RefMsg.MessageItem.TextItem.Text
				}
			}
			msg.QuotedMessage = q
		}
	}

	return msg
}

func detectType(items []MessageItem) ContentType {
	for _, item := range items {
		switch item.Type {
		case ItemImage:
			return ContentImage
		case ItemVoice:
			return ContentVoice
		case ItemFile:
			return ContentFile
		case ItemVideo:
			return ContentVideo
		case ItemToolCallStart:
			return ContentToolCallStart
		case ItemToolCallResult:
			return ContentToolCallResult
		}
	}
	return ContentText
}

func extractText(items []MessageItem) string {
	var parts []string
	for _, item := range items {
		switch item.Type {
		case ItemText:
			if item.TextItem != nil {
				parts = append(parts, item.TextItem.Text)
			}
		case ItemImage:
			if item.ImageItem != nil && item.ImageItem.URL != "" {
				parts = append(parts, item.ImageItem.URL)
			} else {
				parts = append(parts, "[image]")
			}
		case ItemVoice:
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				parts = append(parts, item.VoiceItem.Text)
			} else {
				parts = append(parts, "[voice]")
			}
		case ItemFile:
			if item.FileItem != nil && item.FileItem.FileName != "" {
				parts = append(parts, item.FileItem.FileName)
			} else {
				parts = append(parts, "[file]")
			}
		case ItemVideo:
			parts = append(parts, "[video]")
		}
	}
	return strings.Join(parts, "\n")
}
