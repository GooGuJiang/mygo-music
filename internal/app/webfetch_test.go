package app

import (
	"testing"

	"mygo-music/internal/plugin"
)

// A 403 DataDome JSON payload with a captcha-delivery.com URL is recognized so
// webFetch can show the interactive verification window; other responses are not.
func TestExtractCaptchaURL(t *testing.T) {
	want := "https://geo.captcha-delivery.com/captcha/?initialCid=abc&t=fe"
	got := extractCaptchaURL(plugin.WebFetchResponse{
		Status: 403,
		Text:   `{"url":"` + want + `"}`,
	})
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if got := extractCaptchaURL(plugin.WebFetchResponse{Status: 200, Text: `{"url":"` + want + `"}`}); got != "" {
		t.Fatalf("200 should not trigger verification, got %q", got)
	}
	if got := extractCaptchaURL(plugin.WebFetchResponse{Status: 403, Text: `{"url":"https://evil.example/captcha"}`}); got != "" {
		t.Fatalf("unrelated host should not trigger verification, got %q", got)
	}
	if got := extractCaptchaURL(plugin.WebFetchResponse{Status: 403, Text: `{"url":"http://geo.captcha-delivery.com/captcha/"}`}); got != "" {
		t.Fatalf("non-https URL should not trigger verification, got %q", got)
	}
}
