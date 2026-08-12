package wechatbot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Icatme/wechatbot-go/internal/markdown"
	"github.com/Icatme/wechatbot-go/internal/remote"
	"github.com/Icatme/wechatbot-go/internal/thumb"
)

// Reply sends a text reply to an incoming message.
func (b *Bot) Reply(ctx context.Context, msg *IncomingMessage, text string) error {
	if msg == nil {
		return fmt.Errorf("inbound message is nil")
	}
	sessionGeneration, err := b.incomingSessionGeneration(msg)
	if err != nil {
		return err
	}
	if err := requireContextToken(msg.UserID, msg.ContextToken); err != nil {
		return err
	}
	if err := b.persistContextToken(sessionGeneration, msg.UserID, msg.ContextToken); err != nil {
		if errors.Is(err, ErrReauthRequired) {
			return err
		}
		b.log("warn", "failed to persist context token: %v", err)
	}
	if err := b.sendText(ctx, sessionGeneration, msg.UserID, text, msg.ContextToken); err != nil {
		b.notifyError(ctx, sessionGeneration, msg.UserID, msg.ContextToken, err)
		return err
	}
	return nil
}

// Send sends a text message to a user (requires prior context_token).
func (b *Bot) Send(ctx context.Context, userID, text string) error {
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if err := requireContextToken(userID, ct); err != nil {
		return err
	}
	if err := b.sendText(ctx, sessionGeneration, userID, text, ct); err != nil {
		b.notifyError(ctx, sessionGeneration, userID, ct, err)
		return err
	}
	return nil
}

// SendTyping shows the "typing..." indicator.
func (b *Bot) SendTyping(ctx context.Context, userID string) error {
	creds, configCache, sessionGeneration, err := b.readyConfig(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if err := requireContextToken(userID, ct); err != nil {
		return err
	}
	cfg, err := configCache.GetForUser(ctx, userID, ct)
	if err != nil {
		return b.handleAuthenticatedErrorForSession(sessionGeneration, creds.Token, err)
	}
	creds, err = b.readySession(sessionGeneration)
	if err != nil {
		return err
	}
	if cfg.TypingTicket == "" {
		return nil
	}
	return b.sendTypingForSession(ctx, sessionGeneration, userID, cfg.TypingTicket, 1)
}

// StopTyping cancels the "typing..." indicator.
func (b *Bot) StopTyping(ctx context.Context, userID string) error {
	creds, configCache, sessionGeneration, err := b.readyConfig(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if ct == "" {
		return nil
	}
	cfg, err := configCache.GetForUser(ctx, userID, ct)
	if err != nil {
		return b.handleAuthenticatedErrorForSession(sessionGeneration, creds.Token, err)
	}
	creds, err = b.readySession(sessionGeneration)
	if err != nil {
		return err
	}
	if cfg.TypingTicket == "" {
		return nil
	}
	return b.sendTypingForSession(ctx, sessionGeneration, userID, cfg.TypingTicket, 2)
}

func (b *Bot) sendTypingForSession(ctx context.Context, sessionGeneration uint64, userID, ticket string, status int) error {
	token, sendErr := b.authenticatedRequest(ctx, sessionGeneration, func(requestContext context.Context, baseURL, token string) error {
		return b.client.SendTyping(requestContext, baseURL, token, userID, ticket, status)
	})
	if token == "" {
		return sendErr
	}
	if err := b.handleAuthenticatedErrorForSession(sessionGeneration, token, sendErr); err != nil {
		return err
	}
	_, err := b.readySession(sessionGeneration)
	return err
}

// SendContent describes what to send. Use one of:
//   - SendText("Hello!")
//   - SendImage(data)
//   - SendImageURL("https://example.com/a.png")
//   - SendVideo(data)
//   - SendVideoURL("https://example.com/a.mp4")
//   - SendFile(data, "report.pdf")
//   - SendFileURL("https://example.com/report.pdf", "report.pdf")
type SendContent struct {
	Text     string
	Image    []byte
	Video    []byte
	File     []byte
	FileName string
	Caption  string
	ImageURL string
	VideoURL string
	FileURL  string
}

// resolveRemote downloads remote media when ImageURL/VideoURL/FileURL is set,
// returning a SendContent backed by local bytes.
func (content SendContent) resolveRemote(ctx context.Context, httpClient *http.Client) (SendContent, error) {
	if content.ImageURL != "" {
		data, _, err := remote.DownloadWithClient(ctx, httpClient, content.ImageURL)
		if err != nil {
			return content, fmt.Errorf("download image: %w", err)
		}
		content.Image = data
		content.ImageURL = ""
	}
	if content.VideoURL != "" {
		data, _, err := remote.DownloadWithClient(ctx, httpClient, content.VideoURL)
		if err != nil {
			return content, fmt.Errorf("download video: %w", err)
		}
		content.Video = data
		content.VideoURL = ""
	}
	if content.FileURL != "" {
		data, name, err := remote.DownloadWithClient(ctx, httpClient, content.FileURL)
		if err != nil {
			return content, fmt.Errorf("download file: %w", err)
		}
		content.File = data
		content.FileURL = ""
		if content.FileName == "" {
			content.FileName = name
		}
	}
	return content, nil
}

// SendText creates a text SendContent.
func SendText(text string) SendContent { return SendContent{Text: text} }

// SendImage creates an image SendContent.
func SendImage(data []byte) SendContent { return SendContent{Image: data} }

// SendImageURL creates an image SendContent from a remote URL.
func SendImageURL(url string) SendContent { return SendContent{ImageURL: url} }

// SendVideo creates a video SendContent.
func SendVideo(data []byte) SendContent { return SendContent{Video: data} }

// SendVideoURL creates a video SendContent from a remote URL.
func SendVideoURL(url string) SendContent { return SendContent{VideoURL: url} }

// SendFile creates a file SendContent.
func SendFile(data []byte, fileName string) SendContent {
	return SendContent{File: data, FileName: fileName}
}

// SendFileURL creates a file SendContent from a remote URL.
func SendFileURL(url, fileName string) SendContent {
	return SendContent{FileURL: url, FileName: fileName}
}

// ReplyContent replies with any content type.
func (b *Bot) ReplyContent(ctx context.Context, msg *IncomingMessage, content SendContent) error {
	if msg == nil {
		return fmt.Errorf("inbound message is nil")
	}
	sessionGeneration, err := b.incomingSessionGeneration(msg)
	if err != nil {
		return err
	}
	if err := requireContextToken(msg.UserID, msg.ContextToken); err != nil {
		return err
	}
	if err := b.persistContextToken(sessionGeneration, msg.UserID, msg.ContextToken); err != nil {
		if errors.Is(err, ErrReauthRequired) {
			return err
		}
		b.log("warn", "failed to persist context token: %v", err)
	}
	resolved, err := content.resolveRemote(ctx, b.client.HTTP)
	if err != nil {
		return err
	}
	if err := b.sendContent(ctx, sessionGeneration, msg.UserID, msg.ContextToken, resolved); err != nil {
		b.notifyError(ctx, sessionGeneration, msg.UserID, msg.ContextToken, err)
		return err
	}
	return nil
}

// SendMedia sends any content type to a user.
func (b *Bot) SendMedia(ctx context.Context, userID string, content SendContent) error {
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return err
	}
	ct := b.contextTokens.Get(userID)
	if err := requireContextToken(userID, ct); err != nil {
		return err
	}
	resolved, err := content.resolveRemote(ctx, b.client.HTTP)
	if err != nil {
		return err
	}
	if err := b.sendContent(ctx, sessionGeneration, userID, ct, resolved); err != nil {
		b.notifyError(ctx, sessionGeneration, userID, ct, err)
		return err
	}
	return nil
}

func requireContextToken(userID, contextToken string) error {
	if contextToken == "" {
		return fmt.Errorf("no context_token for user %s", userID)
	}
	return nil
}

func (b *Bot) sendContent(ctx context.Context, sessionGeneration uint64, userID, contextToken string, content SendContent) error {
	if err := requireContextToken(userID, contextToken); err != nil {
		return err
	}

	// Text-only path.
	if content.Text != "" {
		return b.sendText(ctx, sessionGeneration, userID, content.Text, contextToken)
	}

	if _, err := b.readySession(sessionGeneration); err != nil {
		return err
	}

	// Send caption as a separate text message first, then send the media.
	if content.Caption != "" {
		if err := b.sendText(ctx, sessionGeneration, userID, content.Caption, contextToken); err != nil {
			return err
		}
	}

	// Image
	if content.Image != nil {
		thumbData, _ := thumb.FromImage(content.Image)
		if thumbData == nil {
			thumbData = thumb.Placeholder()
		}
		result, err := b.cdnUploadWithThumb(ctx, sessionGeneration, content.Image, thumbData, userID, int(MediaImage))
		if err != nil {
			return err
		}
		imageItem := &ImageItem{
			Media:   &result.Media,
			MidSize: int64(result.EncryptedFileSize),
		}
		if result.ThumbMedia.EncryptQueryParam != "" {
			imageItem.ThumbMedia = &result.ThumbMedia
		}
		_, err = b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type:      ItemImage,
			ImageItem: imageItem,
		}})
		return err
	}

	// Video
	if content.Video != nil {
		// Go has no standard video frame extraction; use a placeholder thumbnail.
		thumbData := thumb.Placeholder()
		result, err := b.cdnUploadWithThumb(ctx, sessionGeneration, content.Video, thumbData, userID, int(MediaVideo))
		if err != nil {
			return err
		}
		videoItem := &VideoItem{
			Media:     &result.Media,
			VideoSize: int64(result.EncryptedFileSize),
		}
		if result.ThumbMedia.EncryptQueryParam != "" {
			videoItem.ThumbMedia = &result.ThumbMedia
		}
		_, err = b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type:      ItemVideo,
			VideoItem: videoItem,
		}})
		return err
	}

	// File (auto-route by extension)
	if content.File != nil {
		fileName := content.FileName
		if fileName == "" {
			fileName = "file.bin"
		}
		cat := categorizeByExtension(fileName)
		if cat == "image" {
			return b.sendContent(ctx, sessionGeneration, userID, contextToken, SendContent{Image: content.File})
		}
		if cat == "video" {
			return b.sendContent(ctx, sessionGeneration, userID, contextToken, SendContent{Video: content.File})
		}
		// Generic file
		result, err := b.cdnUpload(ctx, sessionGeneration, content.File, userID, int(MediaFile))
		if err != nil {
			return err
		}
		_, err = b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type: ItemFile,
			FileItem: &FileItem{
				Media:    &result.Media,
				FileName: fileName,
				Len:      strconv.Itoa(len(content.File)),
			},
		}})
		return err
	}

	// Caption-only is valid: we already sent it above.
	if content.Caption != "" {
		return nil
	}

	return fmt.Errorf("empty SendContent")
}

const maxTextChars = 2000

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".svg": true}
var videoExts = map[string]bool{".mp4": true, ".mov": true, ".webm": true, ".mkv": true, ".avi": true}

func categorizeByExtension(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if imageExts[ext] {
		return "image"
	}
	if videoExts[ext] {
		return "video"
	}
	return "file"
}

func (b *Bot) sendText(ctx context.Context, sessionGeneration uint64, userID, text, contextToken string) error {
	if err := requireContextToken(userID, contextToken); err != nil {
		return err
	}
	if b.opts.StripMarkdown {
		text = markdown.StripMarkdown(text)
	}
	chunks := chunkText(text, maxTextChars)
	for _, chunk := range chunks {
		_, err := b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
			Type:     ItemText,
			TextItem: &TextItem{Text: chunk},
		}})
		if err != nil {
			return err
		}
	}
	return nil
}

// notifyError sends a short error notice to the user when NotifyErrors is enabled.
// Errors are best-effort; failures to send the notice are logged but not returned.
func (b *Bot) notifyError(ctx context.Context, sessionGeneration uint64, userID, contextToken string, err error) {
	if !b.opts.NotifyErrors {
		return
	}
	if requireContextToken(userID, contextToken) != nil {
		return
	}
	if _, readyErr := b.readySession(sessionGeneration); readyErr != nil {
		return
	}
	msg := "⚠️ 消息发送失败，请稍后重试。"
	_, e := b.sendMessage(ctx, sessionGeneration, userID, contextToken, OutboundMessage{Item: MessageItem{
		Type:     ItemText,
		TextItem: &TextItem{Text: msg},
	}})
	if e != nil {
		b.log("warn", "failed to send error notice: %v", e)
	}
}

func chunkText(text string, limit int) []string {
	if limit <= 0 {
		return []string{text}
	}
	if runeLen(text) <= limit {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if runeLen(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		prefix := firstRunes(text, limit)
		cut := len(prefix)
		if idx := strings.LastIndex(prefix, "\n\n"); idx >= 0 && runeLen(prefix[:idx]) > limit*3/10 {
			cut = idx + 2
		} else if idx := strings.LastIndex(prefix, "\n"); idx >= 0 && runeLen(prefix[:idx]) > limit*3/10 {
			cut = idx + 1
		} else if idx := strings.LastIndex(prefix, " "); idx >= 0 && runeLen(prefix[:idx]) > limit*3/10 {
			cut = idx + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

func firstRunes(text string, limit int) string {
	count := 0
	for idx := range text {
		if count == limit {
			return text[:idx]
		}
		count++
	}
	return text
}

func runeLen(text string) int {
	count := 0
	for range text {
		count++
	}
	return count
}
