package wechatbot

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Icatme/wechatbot-go/internal/crypto"
	"github.com/Icatme/wechatbot-go/internal/protocol"
)

const maxDownloadBytes = 100 * 1024 * 1024

// Download downloads media from an incoming message.
// Returns nil if the message has no media. Priority: image > file > video > voice.
func (b *Bot) Download(ctx context.Context, msg *IncomingMessage) (*DownloadedMedia, error) {
	if len(msg.Images) > 0 && msg.Images[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Images[0].Media, msg.Images[0].AESKey)
		if err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: "image"}, nil
	}

	if len(msg.Files) > 0 && msg.Files[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Files[0].Media, "")
		if err != nil {
			return nil, err
		}
		name := msg.Files[0].FileName
		if name == "" {
			name = "file.bin"
		}
		return &DownloadedMedia{Data: data, Type: "file", FileName: name}, nil
	}

	if len(msg.Videos) > 0 && msg.Videos[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Videos[0].Media, "")
		if err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: "video"}, nil
	}

	if len(msg.Voices) > 0 && msg.Voices[0].Media != nil {
		data, err := b.cdnDownload(ctx, msg.Voices[0].Media, "")
		if err != nil {
			return nil, err
		}
		return &DownloadedMedia{Data: data, Type: "voice", Format: "silk"}, nil
	}

	return nil, nil
}

// DownloadRaw downloads and decrypts a raw CDN media reference.
func (b *Bot) DownloadRaw(ctx context.Context, media *CDNMedia, aeskeyOverride string) ([]byte, error) {
	return b.cdnDownload(ctx, media, aeskeyOverride)
}

// Upload uploads data to WeChat CDN without sending a message.
func (b *Bot) Upload(ctx context.Context, data []byte, userID string, mediaType int) (*UploadResult, error) {
	_, sessionGeneration, err := b.readySessionForContext(ctx)
	if err != nil {
		return nil, err
	}
	return b.cdnUpload(ctx, sessionGeneration, data, userID, mediaType)
}

func (b *Bot) cdnDownload(ctx context.Context, media *CDNMedia, aeskeyOverride string) ([]byte, error) {
	if media == nil {
		return nil, fmt.Errorf("missing CDN media")
	}
	downloadURL := media.FullURL
	if downloadURL == "" {
		if media.EncryptQueryParam == "" {
			return nil, fmt.Errorf("missing CDN encrypted_query_param")
		}
		downloadURL = fmt.Sprintf("%s/download?encrypted_query_param=%s",
			protocol.CDNBaseURL, url.QueryEscape(media.EncryptQueryParam))
	}

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdn download request: %w", err)
	}
	resp, err := b.client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdn download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cdn download failed: HTTP %d", resp.StatusCode)
	}

	reader := io.LimitReader(resp.Body, maxDownloadBytes+1)
	ciphertext, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("cdn download read: %w", err)
	}
	if len(ciphertext) > maxDownloadBytes {
		return nil, fmt.Errorf("cdn download exceeds %d bytes", maxDownloadBytes)
	}

	keySource := aeskeyOverride
	if keySource == "" {
		keySource = media.AESKey
	}
	if keySource == "" {
		return nil, fmt.Errorf("no AES key available for decryption")
	}

	aesKey, err := crypto.DecodeAESKey(keySource)
	if err != nil {
		return nil, fmt.Errorf("decode aes key: %w", err)
	}

	return crypto.DecryptAESECB(ciphertext, aesKey)
}

func (b *Bot) cdnUpload(ctx context.Context, sessionGeneration uint64, data []byte, userID string, mediaType int) (*UploadResult, error) {
	return b.cdnUploadWithThumb(ctx, sessionGeneration, data, nil, userID, mediaType)
}

func (b *Bot) cdnUploadWithThumb(ctx context.Context, sessionGeneration uint64, data, thumbData []byte, userID string, mediaType int) (*UploadResult, error) {
	aesKey, err := crypto.GenerateAESKey()
	if err != nil {
		return nil, fmt.Errorf("generate aes key: %w", err)
	}
	ciphertext, err := crypto.EncryptAESECB(data, aesKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	var fileKeyBuf [16]byte
	if _, err := rand.Read(fileKeyBuf[:]); err != nil {
		return nil, fmt.Errorf("generate file key: %w", err)
	}
	fileKey := hex.EncodeToString(fileKeyBuf[:])

	rawMD5 := md5.Sum(data)
	rawMD5Hex := hex.EncodeToString(rawMD5[:])

	thumbReq := protocol.GetUploadURLRequest{
		FileKey:     fileKey,
		MediaType:   mediaType,
		ToUserID:    userID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5Hex,
		FileSize:    len(ciphertext),
		NoNeedThumb: thumbData == nil,
		AESKey:      crypto.EncodeAESKeyHex(aesKey),
	}
	var thumbAESKey []byte
	if thumbData != nil {
		thumbAESKey, err = crypto.GenerateAESKey()
		if err != nil {
			return nil, fmt.Errorf("generate thumb aes key: %w", err)
		}
		thumbCipher, err := crypto.EncryptAESECB(thumbData, thumbAESKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt thumb: %w", err)
		}
		thumbMD5 := md5.Sum(thumbData)
		thumbReq.ThumbRawSize = len(thumbData)
		thumbReq.ThumbFileMD5 = hex.EncodeToString(thumbMD5[:])
		thumbReq.ThumbFileSize = len(thumbCipher)
	}
	uploadResp, requestToken, err := authenticatedRequestResult(b, ctx, sessionGeneration, func(requestContext context.Context, baseURL, token string) (*protocol.GetUploadURLResponse, error) {
		return b.client.GetUploadURL(requestContext, baseURL, token, thumbReq)
	})
	if requestToken == "" {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", b.handleAuthenticatedErrorForSession(sessionGeneration, requestToken, err))
	}
	uploadURL := uploadResp.UploadFullURL
	if uploadURL == "" {
		if uploadResp.UploadParam == "" {
			return nil, fmt.Errorf("getuploadurl did not return upload_param")
		}
		uploadURL = protocol.BuildCDNUploadURL(protocol.CDNBaseURL, uploadResp.UploadParam, fileKey)
	}

	encryptQueryParam, err := b.client.UploadToCDN(ctx, uploadURL, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}

	result := &UploadResult{
		Media: CDNMedia{
			EncryptQueryParam: encryptQueryParam,
			AESKey:            crypto.EncodeAESKeyBase64(aesKey),
			EncryptType:       1,
		},
		AESKey:            aesKey,
		EncryptedFileSize: len(ciphertext),
	}

	if thumbData != nil && uploadResp.ThumbUploadParam != "" {
		thumbCipher, err := crypto.EncryptAESECB(thumbData, thumbAESKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt thumb for upload: %w", err)
		}
		thumbURL := protocol.BuildCDNUploadURL(protocol.CDNBaseURL, uploadResp.ThumbUploadParam, fileKey+"_thumb")
		thumbParam, err := b.client.UploadToCDN(ctx, thumbURL, thumbCipher)
		if err != nil {
			return nil, fmt.Errorf("thumb upload: %w", err)
		}
		if thumbParam != "" {
			result.ThumbMedia = CDNMedia{
				EncryptQueryParam: thumbParam,
				AESKey:            crypto.EncodeAESKeyBase64(thumbAESKey),
				EncryptType:       1,
			}
		}
	}
	if _, err := b.readySession(sessionGeneration); err != nil {
		return nil, err
	}

	return result, nil
}
