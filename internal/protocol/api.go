// Package protocol implements the raw iLink Bot API HTTP calls.
package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Icatme/wechatbot-go/internal/redact"
)

const (
	DefaultBaseURL          = "https://ilinkai.weixin.qq.com"
	ChannelVersion          = "0.4.1"
	maxAPIErrorMessageBytes = 4 * 1024
	// iLink-App-Id header value.
	iLinkAppID = "bot"
)

// CDNBaseURL is the fixed CDN endpoint.
// It is a package-level var so tests can override it.
var CDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

// APIError is returned when the iLink API returns a non-zero ret or HTTP error.
type APIError struct {
	Endpoint   string
	Message    string
	HTTPStatus int
	RetCode    int
	ErrCode    int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ilink api %s: %s (http=%d, ret=%d, errcode=%d)", redact.String(e.Endpoint, nil), redact.String(e.Message, nil), e.HTTPStatus, e.RetCode, e.ErrCode)
}

// Code returns errcode when present, otherwise ret.
func (e *APIError) Code() int {
	if e.ErrCode != 0 {
		return e.ErrCode
	}
	return e.RetCode
}

// IsSessionExpired returns true if this error indicates session timeout.
func (e *APIError) IsSessionExpired() bool {
	return e.RetCode == -14 || e.ErrCode == -14
}

// RandomWechatUIN generates the X-WECHAT-UIN header value.
func RandomWechatUIN() string {
	var buf [4]byte
	rand.Read(buf[:])
	val := binary.BigEndian.Uint32(buf[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(val), 10)))
}

// CommonHeaders returns headers included in both GET and POST requests.
func (c *Client) CommonHeaders() http.Header {
	h := http.Header{}
	h.Set("iLink-App-Id", iLinkAppID)
	h.Set("iLink-App-ClientVersion", clientVersion())
	if c.RouteTag != "" {
		h.Set("SKRouteTag", c.RouteTag)
	}
	return h
}

// AuthHeaders returns the standard iLink POST headers.
func (c *Client) AuthHeaders(token string) http.Header {
	h := c.CommonHeaders()
	h.Set("Content-Type", "application/json")
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("Authorization", "Bearer "+token)
	h.Set("X-WECHAT-UIN", RandomWechatUIN())
	return h
}

// Client wraps HTTP calls to the iLink API.
type Client struct {
	HTTP     *http.Client
	BotAgent string
	RouteTag string
}

// NewClient creates a protocol client with sensible defaults.
func NewClient() *Client {
	return &Client{
		HTTP:     &http.Client{Timeout: 45 * time.Second},
		BotAgent: defaultBotAgent,
	}
}

func (c *Client) baseInfo() map[string]string {
	info := map[string]string{"channel_version": moduleVersionClean()}
	if c.BotAgent != "" {
		info["bot_agent"] = c.BotAgent
	}
	return info
}

// QRCodeResponse from get_bot_qrcode.
type QRCodeResponse struct {
	QRCode       string `json:"qrcode"`
	QRCodeImgURL string `json:"qrcode_img_content"`
}

// QRStatusResponse from get_qrcode_status.
type QRStatusResponse struct {
	Status       string `json:"status"` // wait, scaned, confirmed, expired, scaned_but_redirect, need_verifycode, verify_code_blocked, binded_redirect
	BotToken     string `json:"bot_token,omitempty"`
	BotID        string `json:"ilink_bot_id,omitempty"`
	UserID       string `json:"ilink_user_id,omitempty"`
	BaseURL      string `json:"baseurl,omitempty"`
	RedirectHost string `json:"redirect_host,omitempty"` // set when status is scaned_but_redirect
}

// GetUpdatesResponse from getupdates.
type GetUpdatesResponse struct {
	Ret                  int               `json:"ret"`
	Msgs                 []json.RawMessage `json:"msgs"`
	GetUpdatesBuf        string            `json:"get_updates_buf"`
	ErrCode              int               `json:"errcode,omitempty"`
	ErrMsg               string            `json:"errmsg,omitempty"`
	LongPollingTimeoutMs int               `json:"longpolling_timeout_ms,omitempty"`
}

// GetConfigResponse from getconfig.
type GetConfigResponse struct {
	TypingTicket string `json:"typing_ticket,omitempty"`
	Ret          int    `json:"ret,omitempty"`
}

// GetQRCode requests a new QR code for login.
// localTokenList may contain up to 10 recent bot tokens to speed up re-login.
func (c *Client) GetQRCode(ctx context.Context, baseURL string, localTokenList []string) (*QRCodeResponse, error) {
	u := baseURL + "/ilink/bot/get_bot_qrcode?bot_type=3"
	body := map[string]interface{}{}
	if len(localTokenList) > 0 {
		if len(localTokenList) > 10 {
			localTokenList = localTokenList[:10]
		}
		body["local_token_list"] = localTokenList
	}
	var bodyReader io.Reader
	if len(body) > 0 {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("get_bot_qrcode encode: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("get_bot_qrcode request: %w", redact.Error(err))
	}
	for k, v := range c.CommonHeaders() {
		req.Header[k] = v
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get_bot_qrcode: %w", redact.Error(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, responseAPIError("/ilink/bot/get_bot_qrcode", resp.StatusCode, raw)
	}
	var result QRCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("get_bot_qrcode decode: %w", err)
	}
	return &result, nil
}

// PollQRStatus polls the QR code scan status.
// If verifyCode is non-empty, it is sent on the next poll after a need_verifycode response.
func (c *Client) PollQRStatus(ctx context.Context, baseURL, qrcode, verifyCode string) (*QRStatusResponse, error) {
	u := baseURL + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	if verifyCode != "" {
		u += "&verify_code=" + url.QueryEscape(verifyCode)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("get_qrcode_status request: %w", redact.Error(err))
	}
	for k, v := range c.CommonHeaders() {
		req.Header[k] = v
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get_qrcode_status: %w", redact.Error(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, responseAPIError("/ilink/bot/get_qrcode_status", resp.StatusCode, raw)
	}
	var result QRStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("get_qrcode_status decode: %w", err)
	}
	return &result, nil
}

// apiPost sends a POST to the iLink API and parses the response.
func (c *Client) apiPost(ctx context.Context, baseURL, endpoint, token string, body interface{}, timeout time.Duration) (json.RawMessage, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("%s encode: %w", endpoint, err)
	}
	u := baseURL + endpoint
	httpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(httpCtx, "POST", u, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%s request: %w", endpoint, redact.Error(err))
	}
	for k, v := range c.AuthHeaders(token) {
		req.Header[k] = v
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, redact.Error(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s read response: %w", endpoint, err)
	}
	var check apiEnvelope
	if err := json.Unmarshal(raw, &check); err != nil {
		if resp.StatusCode >= 400 {
			return nil, responseAPIError(endpoint, resp.StatusCode, raw)
		}
		return nil, fmt.Errorf("%s decode response: %w", endpoint, err)
	}
	if resp.StatusCode >= 400 {
		return nil, newAPIError(endpoint, resp.StatusCode, raw, check)
	}
	if check.Ret != 0 || check.ErrCode != 0 {
		return nil, newAPIError(endpoint, resp.StatusCode, raw, check)
	}

	return json.RawMessage(raw), nil
}

type apiEnvelope struct {
	Ret     int    `json:"ret"`
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func responseAPIError(endpoint string, status int, raw []byte) *APIError {
	var envelope apiEnvelope
	_ = json.Unmarshal(raw, &envelope)
	return newAPIError(endpoint, status, raw, envelope)
}

func newAPIError(endpoint string, status int, raw []byte, envelope apiEnvelope) *APIError {
	message := envelope.ErrMsg
	if message == "" {
		message = strings.TrimSpace(strings.ToValidUTF8(string(raw), "\uFFFD"))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	message = sanitizeAPIErrorMessage(message)
	return &APIError{
		Endpoint:   endpoint,
		Message:    message,
		HTTPStatus: status,
		RetCode:    envelope.Ret,
		ErrCode:    envelope.ErrCode,
	}
}

func sanitizeAPIErrorMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	return truncateAPIErrorMessage(redact.String(message, nil))
}

func truncateAPIErrorMessage(message string) string {
	if len(message) <= maxAPIErrorMessageBytes {
		return message
	}
	const marker = "…(truncated)"
	end := maxAPIErrorMessageBytes - len(marker)
	for end > 0 && !utf8.ValidString(message[:end]) {
		end--
	}
	return message[:end] + marker
}

// GetUpdates performs a long-poll for new messages.
// timeout controls the client-side HTTP timeout for this request.
func (c *Client) GetUpdates(ctx context.Context, baseURL, token, cursor string, timeout time.Duration) (*GetUpdatesResponse, error) {
	body := map[string]interface{}{
		"get_updates_buf": cursor,
		"base_info":       c.baseInfo(),
	}
	raw, err := c.apiPost(ctx, baseURL, "/ilink/bot/getupdates", token, body, timeout)
	if err != nil {
		return nil, err
	}
	var result GetUpdatesResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("getupdates decode: %w", err)
	}
	return &result, nil
}

// SendMessage sends a message through the iLink API.
func (c *Client) SendMessage(ctx context.Context, baseURL, token string, msg interface{}) error {
	body := map[string]interface{}{
		"msg":       msg,
		"base_info": c.baseInfo(),
	}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/sendmessage", token, body, 15*time.Second)
	return err
}

// GetConfig gets the typing ticket for a user.
func (c *Client) GetConfig(ctx context.Context, baseURL, token, userID, contextToken string) (*GetConfigResponse, error) {
	body := map[string]interface{}{
		"ilink_user_id": userID,
		"context_token": contextToken,
		"base_info":     c.baseInfo(),
	}
	raw, err := c.apiPost(ctx, baseURL, "/ilink/bot/getconfig", token, body, 15*time.Second)
	if err != nil {
		return nil, err
	}
	var result GetConfigResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("getconfig decode: %w", err)
	}
	return &result, nil
}

// SendTyping sends or cancels the typing indicator.
func (c *Client) SendTyping(ctx context.Context, baseURL, token, userID, ticket string, status int) error {
	body := map[string]interface{}{
		"ilink_user_id": userID,
		"typing_ticket": ticket,
		"status":        status,
		"base_info":     c.baseInfo(),
	}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/sendtyping", token, body, 15*time.Second)
	return err
}

// NotifyStart tells WeChat that this bot client is starting.
func (c *Client) NotifyStart(ctx context.Context, baseURL, token string) error {
	body := map[string]interface{}{
		"base_info": c.baseInfo(),
	}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/msg/notifystart", token, body, 10*time.Second)
	return err
}

// NotifyStop tells WeChat that this bot client is stopping.
func (c *Client) NotifyStop(ctx context.Context, baseURL, token string) error {
	body := map[string]interface{}{
		"base_info": c.baseInfo(),
	}
	_, err := c.apiPost(ctx, baseURL, "/ilink/bot/msg/notifystop", token, body, 10*time.Second)
	return err
}

// GetUploadURLRequest holds parameters for getuploadurl.
type GetUploadURLRequest struct {
	FileKey       string `json:"filekey"`
	MediaType     int    `json:"media_type"`
	ToUserID      string `json:"to_user_id"`
	RawSize       int    `json:"rawsize"`
	RawFileMD5    string `json:"rawfilemd5"`
	FileSize      int    `json:"filesize"`
	ThumbRawSize  int    `json:"thumb_rawsize,omitempty"`
	ThumbFileMD5  string `json:"thumb_rawfilemd5,omitempty"`
	ThumbFileSize int    `json:"thumb_filesize,omitempty"`
	NoNeedThumb   bool   `json:"no_need_thumb,omitempty"`
	AESKey        string `json:"aeskey,omitempty"`
}

// GetUploadURLResponse from getuploadurl.
type GetUploadURLResponse struct {
	UploadParam      string `json:"upload_param"`
	ThumbUploadParam string `json:"thumb_upload_param,omitempty"`
	// Complete upload URL returned by server; when set, use directly instead of building from UploadParam.
	UploadFullURL string `json:"upload_full_url,omitempty"`
}

// GetUploadURL requests an upload URL for CDN media upload.
func (c *Client) GetUploadURL(ctx context.Context, baseURL, token string, req GetUploadURLRequest) (*GetUploadURLResponse, error) {
	body := map[string]interface{}{
		"filekey":          req.FileKey,
		"media_type":       req.MediaType,
		"to_user_id":       req.ToUserID,
		"rawsize":          req.RawSize,
		"rawfilemd5":       req.RawFileMD5,
		"filesize":         req.FileSize,
		"thumb_rawsize":    req.ThumbRawSize,
		"thumb_rawfilemd5": req.ThumbFileMD5,
		"thumb_filesize":   req.ThumbFileSize,
		"no_need_thumb":    req.NoNeedThumb,
		"aeskey":           req.AESKey,
		"base_info":        c.baseInfo(),
	}
	raw, err := c.apiPost(ctx, baseURL, "/ilink/bot/getuploadurl", token, body, 15*time.Second)
	if err != nil {
		return nil, err
	}
	var result GetUploadURLResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("getuploadurl decode: %w", err)
	}
	return &result, nil
}

// UploadToCDN uploads encrypted bytes to the CDN with retry (up to 3 attempts).
// Returns the download encrypted_query_param from the x-encrypted-param header.
// Client errors (4xx) abort immediately; server errors retry.
func (c *Client) UploadToCDN(ctx context.Context, cdnURL string, ciphertext []byte) (string, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "POST", cdnURL, bytes.NewReader(ciphertext))
		if err != nil {
			return "", fmt.Errorf("CDN upload request: %w", redact.Error(err))
		}
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("CDN upload attempt %d: %w", attempt, redact.Error(err))
			continue
		}
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			errMsg := resp.Header.Get("x-error-message")
			resp.Body.Close()
			if errMsg == "" {
				errMsg = fmt.Sprintf("HTTP %d", resp.StatusCode)
			}
			return "", fmt.Errorf("CDN upload client error %d: %s", resp.StatusCode, redact.String(errMsg, nil))
		}
		if resp.StatusCode != 200 {
			errMsg := resp.Header.Get("x-error-message")
			resp.Body.Close()
			lastErr = fmt.Errorf("CDN upload server error %d: %s", resp.StatusCode, redact.String(errMsg, nil))
			continue
		}

		downloadParam := resp.Header.Get("x-encrypted-param")
		resp.Body.Close()
		if downloadParam == "" {
			lastErr = fmt.Errorf("CDN upload response missing x-encrypted-param header")
			continue
		}
		return downloadParam, nil
	}
	return "", fmt.Errorf("CDN upload failed after %d attempts: %w", maxRetries, lastErr)
}

// BuildCDNUploadURL constructs a CDN upload URL from params.
func BuildCDNUploadURL(cdnBaseURL, uploadParam, filekey string) string {
	return cdnBaseURL + "/upload?encrypted_query_param=" + url.QueryEscape(uploadParam) + "&filekey=" + url.QueryEscape(filekey)
}

// BuildMessage creates one outbound message payload. The caller owns ClientID
// generation so an uncertain send can be retried with the same identity.
func BuildMessage(userID, contextToken, clientID, runID string, item interface{}) map[string]interface{} {
	msg := map[string]interface{}{
		"from_user_id":  "",
		"to_user_id":    userID,
		"client_id":     clientID,
		"message_type":  2,
		"message_state": 2,
		"context_token": contextToken,
		"item_list":     []interface{}{item},
	}
	if runID != "" {
		msg["run_id"] = runID
	}
	return msg
}
