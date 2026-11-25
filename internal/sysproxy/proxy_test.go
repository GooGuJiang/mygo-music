package sysproxy

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// Parsing single-host and per-scheme Windows ProxyServer strings picks the right proxy URL for http and https requests.
func TestParseProxyServer(t *testing.T) {
	cases := []struct {
		server string
		scheme string
		want   string
	}{
		{"127.0.0.1:7890", "https", "http://127.0.0.1:7890"},
		{"http://127.0.0.1:7890", "https", "http://127.0.0.1:7890"},
		{"http=127.0.0.1:7890;https=127.0.0.1:7891", "http", "http://127.0.0.1:7890"},
		{"http=127.0.0.1:7890;https=127.0.0.1:7891", "https", "http://127.0.0.1:7891"},
		{"socks=127.0.0.1:7891", "https", "socks5://127.0.0.1:7891"},
		{"PROXY 127.0.0.1:7890; DIRECT", "https", "http://127.0.0.1:7890"},
		{"DIRECT", "https", ""},
	}
	for _, tc := range cases {
		got, err := parseProxyServer(tc.server, tc.scheme)
		if err != nil {
			t.Fatalf("parseProxyServer(%q, %q) error: %v", tc.server, tc.scheme, err)
		}
		gotStr := ""
		if got != nil {
			gotStr = got.String()
		}
		if gotStr != tc.want {
			t.Errorf("parseProxyServer(%q, %q) = %q, want %q", tc.server, tc.scheme, gotStr, tc.want)
		}
	}
}

// Windows ProxyOverride rules bypass loopback, <local> hostnames, and wildcard patterns while proxying external domains.
func TestResolveConfigBypass(t *testing.T) {
	cfg := config{
		Enabled:  true,
		Server:   "127.0.0.1:7890",
		Override: "*zhihu.com;localhost;*.local;127.*;192.168.*;<local>",
	}
	check := func(rawURL, want string) {
		t.Helper()
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolveConfig(cfg, &http.Request{URL: u})
		if err != nil {
			t.Fatalf("resolveConfig(%q) error: %v", rawURL, err)
		}
		gotStr := ""
		if got != nil {
			gotStr = got.String()
		}
		if gotStr != want {
			t.Errorf("resolveConfig(%q) = %q, want %q", rawURL, gotStr, want)
		}
	}

	check("https://soundcloud.com/", "http://127.0.0.1:7890")
	check("https://api-v2.soundcloud.com/me", "http://127.0.0.1:7890")
	check("http://127.0.0.1:23456/", "")
	check("http://localhost:8080/", "")
	check("http://[::1]:8080/", "")
	check("https://www.zhihu.com/", "")
	check("http://printer.local/", "")
	check("http://192.168.1.1/", "")
	check("http://intranet/", "")
}

// Child process proxy env propagation populates http_proxy, https_proxy and no_proxy, and respects existing proxy env vars.
func TestEnvPropagation(t *testing.T) {
	cfg := config{
		Enabled:  true,
		Server:   "127.0.0.1:7890",
		Override: "*zhihu.com;localhost;*.local;127.*;<local>",
	}
	extra := envFromConfig(cfg)
	for _, want := range []string{
		"http_proxy=http://127.0.0.1:7890",
		"HTTP_PROXY=http://127.0.0.1:7890",
		"https_proxy=http://127.0.0.1:7890",
		"HTTPS_PROXY=http://127.0.0.1:7890",
		"no_proxy=localhost,127.0.0.1,::1,zhihu.com,.local",
		"NO_PROXY=localhost,127.0.0.1,::1,zhihu.com,.local",
	} {
		if !slices.Contains(extra, want) {
			t.Errorf("envFromConfig missing %q in %v", want, extra)
		}
	}

	existing := []string{"PATH=C:\\Windows", "HTTPS_PROXY=http://127.0.0.1:9999"}
	got := Env(existing)
	if !slices.Equal(got, existing) {
		t.Errorf("Env overwrote explicit proxy env: got %v, want %v", got, existing)
	}
}

// Explicit proxy environment variables take precedence over the OS proxy settings.
func TestProxyEnvPrecedence(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")
	req, _ := http.NewRequest("GET", "https://soundcloud.com/", nil)
	got, err := Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.String() != "http://127.0.0.1:9999" {
		t.Fatalf("Proxy = %v, want http://127.0.0.1:9999", got)
	}
}
