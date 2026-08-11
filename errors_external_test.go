package wechatbot_test

import (
	"errors"
	"testing"

	wechatbot "github.com/Icatme/wechatbot-go"
)

func TestPublicAPIErrorSupportsErrorsAs(t *testing.T) {
	want := &wechatbot.APIError{
		Endpoint:   "/ilink/bot/sendmessage",
		HTTPStatus: 401,
		RetCode:    -14,
		Message:    "expired",
	}
	var got *wechatbot.APIError
	if !errors.As(want, &got) || got.Code() != -14 || !got.IsSessionExpired() {
		t.Fatalf("public API error = %+v", got)
	}
}
