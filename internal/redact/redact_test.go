package redact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestStringRedactsNestedSensitiveData(t *testing.T) {
	input := `{"status":"failed","ret":-2,"Proxy-Authorization":"proxy-secret","result":{"proxy_authorization":"proxy-underscore-secret","qrcode":"qr-secret","qrcode_img_content":"qr-url-secret","verify_code":"123456","context_token":"ctx-secret","typing_ticket":"ticket-secret","client_secret":"credential-secret","upload_param":"upload-secret","thumb_upload_param":"thumb-secret","upload_full_url":"https://cdn.example/upload?signature=url-secret","items":[{"aes_key":"aes-secret","encrypted_query_param":"cdn-secret","full_url":"https://cdn.example/download?signature=full-url-secret"}]},"local_token_list":["token-a","token-b"]}`
	got := String(input, nil)

	for _, secret := range []string{"proxy-secret", "proxy-underscore-secret", "qr-secret", "qr-url-secret", "123456", "ctx-secret", "ticket-secret", "credential-secret", "upload-secret", "thumb-secret", "url-secret", "aes-secret", "cdn-secret", "full-url-secret", "token-a", "token-b"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted JSON contains %q: %s", secret, got)
		}
	}
	for _, diagnostic := range []string{`"status":"failed"`, `"ret":-2`, `"items"`} {
		if !strings.Contains(got, diagnostic) {
			t.Errorf("redacted JSON lost %q: %s", diagnostic, got)
		}
	}
}

func TestStringBestEffortRedactsMalformedJSONAndURLs(t *testing.T) {
	input := "partial={\"bot_token\":\"token-secret\",\"local_token_list\":[\"old-a\",\"old-b\"] request=https://cdn.example/download?unknown_signature=signed-secret&part=2 authorization=Bearer bearer-secret x-encrypted-param=cdn-assignment-secret\nx_encrypted_param: cdn-header-secret status=502"
	got := String(input, nil)

	for _, secret := range []string{"token-secret", "old-a", "old-b", "signed-secret", "bearer-secret", "cdn-assignment-secret", "cdn-header-secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted text contains %q: %s", secret, got)
		}
	}
	for _, diagnostic := range []string{"partial=", "cdn.example/download", "status=502"} {
		if !strings.Contains(got, diagnostic) {
			t.Errorf("redacted text lost %q: %s", diagnostic, got)
		}
	}
}

func TestStringBestEffortRedactsNestedMalformedJSON(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		secret string
	}{
		{
			name:   "nested object",
			input:  `partial={"details":{"status":"failed","bot_token":"object-secret"`,
			secret: "object-secret",
		},
		{
			name:   "nested array",
			input:  `partial={"items":[{"status":"failed"},{"nested":{"aes_key":"array-secret"}}`,
			secret: "array-secret",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := String(test.input, nil)
			if strings.Contains(got, test.secret) {
				t.Fatalf("nested malformed JSON leaked %q: %s", test.secret, got)
			}
			if !strings.Contains(got, `"status":"failed"`) || !strings.Contains(got, `"***"`) {
				t.Fatalf("nested malformed JSON lost diagnostics or marker: %s", got)
			}
		})
	}
}

func TestURLMalformedDoesNotRecurse(t *testing.T) {
	const malformed = "https://host.example/%zz"
	if got := URL(malformed); got != malformed {
		t.Fatalf("URL(%q) = %q", malformed, got)
	}
}

func TestURLMalformedRedactsUserinfoAndQuery(t *testing.T) {
	tests := []string{
		"https://user:pass@host.example/%zz",
		"https://user:pass@host.example/%zz?signature=signed-secret",
		"//user:pass@host.example/%zz",
	}
	for _, malformed := range tests {
		got := URL(malformed)
		for _, secret := range []string{"user", "pass", "signed-secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("URL(%q) leaked %q: %q", malformed, secret, got)
			}
		}
		if !strings.Contains(got, "host.example/%zz") {
			t.Fatalf("URL(%q) lost location diagnostics: %q", malformed, got)
		}
	}
}

func TestStringRedactsBearerAssignment(t *testing.T) {
	got := String("authorization=Bearer token-secret status=401", nil)
	if strings.Contains(got, "token-secret") || !strings.Contains(got, "authorization=***") {
		t.Fatalf("Bearer assignment was not fully redacted: %s", got)
	}
	if !strings.Contains(got, "status=401") {
		t.Fatalf("Bearer assignment redaction lost diagnostics: %s", got)
	}
}

func TestStringRedactsAuthorizationSchemes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "Basic assignment",
			input: "authorization=Basic basic-secret status=401",
			want:  "authorization=*** status=401",
		},
		{
			name:  "custom header scheme",
			input: "Authorization: Custom-Scheme header-secret\nstatus=403",
			want:  "Authorization: ***\nstatus=403",
		},
		{
			name:  "Digest assignment parameters",
			input: "authorization=Digest username=user,response=digest-secret status=401",
			want:  "authorization=*** status=401",
		},
		{
			name:  "double quoted Bearer assignment",
			input: `authorization="Bearer token-secret" status=401`,
			want:  "authorization=*** status=401",
		},
		{
			name:  "single quoted Basic assignment",
			input: "authorization='Basic basic-secret' status=403",
			want:  "authorization=*** status=403",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := String(test.input, nil); got != test.want {
				t.Fatalf("String(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestStringRedactsSchemeLessAssignmentsBeforeDiagnostics(t *testing.T) {
	tests := []string{
		"bot_token=token-secret status=401",
		"authorization=token-secret status=401",
	}
	for _, input := range tests {
		got := String(input, nil)
		if strings.Contains(got, "token-secret") || !strings.Contains(got, "status=401") {
			t.Fatalf("scheme-less assignment was unsafe or lost diagnostics: %s", got)
		}
	}
}

func TestStringRedactsCompleteSensitiveAssignmentValues(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "double quoted value",
			input: `password="correct horse battery staple" status=401`,
			want:  "password=*** status=401",
		},
		{
			name:  "comma in value",
			input: "password=abc,def status=401",
			want:  "password=*** status=401",
		},
		{
			name:  "comma and equals in value",
			input: "password=abc,def=secret status=401",
			want:  "password=*** status=401",
		},
		{
			name:  "semicolon in value",
			input: "password=abc;def status=401",
			want:  "password=*** status=401",
		},
		{
			name:  "header value",
			input: `password: "correct horse battery staple" status=401`,
			want:  "password: *** status=401",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := String(test.input, nil); got != test.want {
				t.Fatalf("String(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestStringRedactsSchemeRelativeURL(t *testing.T) {
	got := String("fetch //user:pass@proxy.example/%zz?signature=signed-secret status=502", nil)
	for _, secret := range []string{"user", "pass", "signed-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("scheme-relative URL leaked %q: %s", secret, got)
		}
	}
	for _, diagnostic := range []string{"proxy.example/%zz", "status=502"} {
		if !strings.Contains(got, diagnostic) {
			t.Fatalf("scheme-relative URL lost %q: %s", diagnostic, got)
		}
	}
}

type credentialError struct {
	message string
}

func (e *credentialError) Error() string { return e.message }

func TestErrorDoesNotRetainUnsafeLeaf(t *testing.T) {
	original := &credentialError{message: `password="correct horse battery staple"`}
	got := Error(original)
	if strings.Contains(got.Error(), "correct horse") {
		t.Fatalf("sanitized error leaked credential: %v", got)
	}
	var credentialErr *credentialError
	if errors.As(got, &credentialErr) || errors.Is(got, original) || errors.Unwrap(got) != nil {
		t.Fatalf("sanitized error retained unsafe leaf: %#v", got)
	}
}

func TestErrorPreservesSafeNestedCause(t *testing.T) {
	cause := errors.New("connection refused")
	original := fmt.Errorf(`password="correct horse battery staple": %w`, cause)
	got := Error(original)
	if strings.Contains(got.Error(), "correct horse") || !errors.Is(got, cause) {
		t.Fatalf("sanitized error was unsafe or lost safe cause: %v", got)
	}
	if errors.Is(got, original) {
		t.Fatal("sanitized error retained unsafe wrapper")
	}
}

func TestErrorSanitizesURLErrorAndPreservesTraversal(t *testing.T) {
	cause := errors.New("connection refused")
	original := &url.Error{
		Op:  "Get",
		URL: "https://api.example/status?qrcode=qr-secret&verify_code=code-secret",
		Err: fmt.Errorf("transport: %w", cause),
	}

	got := Error(original)
	if strings.Contains(got.Error(), "qr-secret") || strings.Contains(got.Error(), "code-secret") {
		t.Fatalf("sanitized error leaked URL: %v", got)
	}
	if !strings.Contains(got.Error(), "api.example/status") || !errors.Is(got, cause) {
		t.Fatalf("sanitized error lost diagnostics or cause: %v", got)
	}
	var urlErr *url.Error
	if !errors.As(got, &urlErr) || strings.Contains(urlErr.URL, "qr-secret") {
		t.Fatalf("errors.As returned unsafe URL error: %+v", urlErr)
	}
	if urlErr == original {
		t.Fatal("errors.As reached the original URL error")
	}
	if errors.Is(got, original) {
		t.Fatal("sanitized error retained the original URL error in its chain")
	}
}

func TestErrorSanitizesWrappedURLErrorAndPreservesTraversal(t *testing.T) {
	cause := errors.New("connection refused")
	original := &url.Error{
		Op:  "Get",
		URL: "https://api.example/status?qrcode=qr-secret&verify_code=code-secret",
		Err: cause,
	}
	wrapped := fmt.Errorf("proxy exchange failed: %w", original)

	got := Error(wrapped)
	if strings.Contains(got.Error(), "qr-secret") || strings.Contains(got.Error(), "code-secret") {
		t.Fatalf("sanitized wrapped error leaked URL: %v", got)
	}
	if !strings.Contains(got.Error(), "proxy exchange failed") || !strings.Contains(got.Error(), "api.example/status") {
		t.Fatalf("sanitized wrapped error lost outer diagnostics: %v", got)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("sanitized wrapped error lost underlying cause: %v", got)
	}
	var urlErr *url.Error
	if !errors.As(got, &urlErr) || strings.Contains(urlErr.URL, "qr-secret") || strings.Contains(urlErr.URL, "code-secret") {
		t.Fatalf("errors.As returned unsafe URL error: %+v", urlErr)
	}
	if urlErr == original {
		t.Fatal("errors.As reached the original URL error")
	}
	if errors.Is(got, original) {
		t.Fatal("sanitized wrapped error retained the original URL error in its chain")
	}
}

func TestErrorSanitizesJoinedURLErrorAndPreservesCauses(t *testing.T) {
	transportCause := errors.New("connection refused")
	otherCause := errors.New("fallback failed")
	original := &url.Error{
		Op:  "Get",
		URL: "https://api.example/status?qrcode=qr-secret",
		Err: transportCause,
	}
	joined := errors.Join(fmt.Errorf("primary transport: %w", original), otherCause)

	got := Error(fmt.Errorf("request failed: %w", joined))
	if strings.Contains(got.Error(), "qr-secret") || !strings.Contains(got.Error(), "request failed") || !strings.Contains(got.Error(), "primary transport") {
		t.Fatalf("joined error was unsafe or lost diagnostics: %v", got)
	}
	if !errors.Is(got, transportCause) || !errors.Is(got, otherCause) || errors.Is(got, original) {
		t.Fatalf("joined error lost safe causes or retained original: %v", got)
	}
	var urlErr *url.Error
	if !errors.As(got, &urlErr) || urlErr == original || strings.Contains(urlErr.URL, "qr-secret") {
		t.Fatalf("errors.As returned unsafe joined URL error: %+v", urlErr)
	}
}
