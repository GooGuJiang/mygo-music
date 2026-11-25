// Package sysproxy resolves the system HTTP proxy when proxy environment
// variables are not set, so requests and spawned helper programs reach the
// internet through the user's configured system proxy on Windows.
package sysproxy

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
)

type config struct {
	Enabled       bool
	Server        string
	Override      string
	AutoConfigURL string
}

var installOnce sync.Once

// Install configures http.DefaultTransport to fall back to the system proxy
// when proxy environment variables are unset.
func Install() {
	installOnce.Do(func() {
		if t, ok := http.DefaultTransport.(*http.Transport); ok {
			t.Proxy = Proxy
		}
	})
}

// NewTransport returns a clone of http.DefaultTransport that honors Proxy.
func NewTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := t.Clone()
		cloned.Proxy = Proxy
		return cloned
	}
	return &http.Transport{Proxy: Proxy}
}

// Proxy returns the proxy URL for req, honoring standard proxy environment
// variables first and falling back to the OS system proxy when none are set.
func Proxy(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	if hasProxyEnv() {
		return http.ProxyFromEnvironment(req)
	}
	if isLoopbackHost(req.URL.Hostname()) {
		return nil, nil
	}
	return systemProxy(req)
}

// Env returns base (or os.Environ() when nil) augmented with proxy environment
// variables derived from the OS system proxy when no proxy env vars are set.
func Env(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	if envHasProxy(base) {
		return base
	}
	extra := systemProxyEnv()
	if len(extra) == 0 {
		return base
	}
	out := slices.Clone(base)
	return append(out, extra...)
}

func hasProxyEnv() bool {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

func envHasProxy(env []string) bool {
	for _, entry := range env {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(val) == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "http_proxy", "https_proxy", "all_proxy":
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "" || h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func resolveConfig(cfg config, req *http.Request) (*url.URL, error) {
	if !cfg.Enabled || strings.TrimSpace(cfg.Server) == "" {
		return nil, nil
	}
	if shouldBypass(req.URL.Hostname(), req.URL.Port(), cfg.Override) {
		return nil, nil
	}
	return parseProxyServer(cfg.Server, req.URL.Scheme)
}

func envFromConfig(cfg config) []string {
	if !cfg.Enabled || strings.TrimSpace(cfg.Server) == "" {
		return nil
	}
	httpProxy, _ := parseProxyServer(cfg.Server, "http")
	httpsProxy, _ := parseProxyServer(cfg.Server, "https")
	return envFromProxyURLs(httpProxy, httpsProxy, cfg.Override)
}

func envFromProxyURLs(httpProxy, httpsProxy *url.URL, override string) []string {
	var httpStr, httpsStr, allStr string
	if isHTTPProxyURL(httpProxy) {
		httpStr = httpProxy.String()
	}
	if isHTTPProxyURL(httpsProxy) {
		httpsStr = httpsProxy.String()
	}
	// Tools such as ffmpeg read http_proxy even when fetching https:// URLs.
	if httpStr == "" && httpsStr != "" {
		httpStr = httpsStr
	}
	if httpsStr == "" && httpStr != "" {
		httpsStr = httpStr
	}
	if httpStr == "" && httpsStr == "" {
		if httpsProxy != nil {
			allStr = httpsProxy.String()
		} else if httpProxy != nil {
			allStr = httpProxy.String()
		}
	}
	if httpStr == "" && httpsStr == "" && allStr == "" {
		return nil
	}
	noProxy := overrideToNoProxy(override)
	var out []string
	if httpStr != "" {
		out = append(out, "http_proxy="+httpStr, "HTTP_PROXY="+httpStr)
	}
	if httpsStr != "" {
		out = append(out, "https_proxy="+httpsStr, "HTTPS_PROXY="+httpsStr)
	}
	if allStr != "" {
		out = append(out, "all_proxy="+allStr, "ALL_PROXY="+allStr)
	}
	if noProxy != "" {
		out = append(out, "no_proxy="+noProxy, "NO_PROXY="+noProxy)
	}
	return out
}

func isHTTPProxyURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return s == "http" || s == "https"
}

// parseProxyServer parses a Windows ProxyServer value (either "host:port",
// "scheme://host:port", or "http=host:port;https=host:port;socks=host:port")
// for the target request scheme.
func parseProxyServer(server, scheme string) (*url.URL, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return nil, nil
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme == "" {
		scheme = "http"
	}

	if !strings.Contains(server, "=") {
		// May be a single entry or a semicolon-separated list from WinHTTP.
		for _, part := range strings.Split(server, ";") {
			u, err := parseProxyEndpoint(part, "http")
			if err == nil && u != nil {
				return u, nil
			}
		}
		return nil, nil
	}

	byScheme := map[string]string{}
	for _, part := range strings.Split(server, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			byScheme[k] = v
		}
	}

	candidates := []struct {
		key           string
		defaultScheme string
	}{
		{scheme, "http"},
		{"http", "http"},
		{"https", "http"},
		{"socks", "socks5"},
		{"socks5", "socks5"},
	}
	for _, c := range candidates {
		if raw, ok := byScheme[c.key]; ok {
			if u, err := parseProxyEndpoint(raw, c.defaultScheme); err == nil && u != nil {
				return u, nil
			}
		}
	}
	return nil, nil
}

func parseProxyEndpoint(raw, defaultScheme string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, nil
	}
	upper := strings.ToUpper(s)
	switch {
	case upper == "DIRECT":
		return nil, nil
	case strings.HasPrefix(upper, "PROXY "):
		s = strings.TrimSpace(s[len("PROXY "):])
	case strings.HasPrefix(upper, "HTTPS "):
		s = strings.TrimSpace(s[len("HTTPS "):])
		defaultScheme = "https"
	case strings.HasPrefix(upper, "SOCKS5 "):
		s = strings.TrimSpace(s[len("SOCKS5 "):])
		defaultScheme = "socks5"
	case strings.HasPrefix(upper, "SOCKS "):
		s = strings.TrimSpace(s[len("SOCKS "):])
		defaultScheme = "socks5"
	}
	if s == "" {
		return nil, nil
	}
	if !strings.Contains(s, "://") {
		s = defaultScheme + "://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, err
	}
	return u, nil
}

func shouldBypass(host, port, override string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if isLoopbackHost(h) {
		return true
	}
	if strings.TrimSpace(override) == "" {
		return false
	}
	for _, raw := range strings.FieldsFunc(override, func(r rune) bool { return r == ';' || r == ',' }) {
		rule := strings.ToLower(strings.TrimSpace(raw))
		if rule == "" {
			continue
		}
		if rule == "<local>" {
			if !strings.Contains(h, ".") && !strings.Contains(h, ":") {
				return true
			}
			continue
		}
		if idx := strings.Index(rule, "://"); idx >= 0 {
			rule = rule[idx+3:]
		}
		if idx := strings.IndexByte(rule, '/'); idx >= 0 {
			rule = rule[:idx]
		}
		ruleHost := rule
		rulePort := ""
		if strings.Count(rule, ":") == 1 {
			if rh, rp, ok := strings.Cut(rule, ":"); ok {
				ruleHost, rulePort = rh, rp
			}
		}
		if rulePort != "" && rulePort != "*" && port != "" && rulePort != port {
			continue
		}
		if wildcardMatch(ruleHost, h) {
			return true
		}
	}
	return false
}

// wildcardMatch matches s against a case-insensitive glob pattern supporting * and ?.
func wildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	starIdx, matchIdx := -1, 0
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			pi++
			si++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
		} else {
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

func overrideToNoProxy(override string) string {
	entries := []string{"localhost", "127.0.0.1", "::1"}
	seen := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, raw := range strings.FieldsFunc(override, func(r rune) bool { return r == ';' || r == ',' }) {
		rule := strings.ToLower(strings.TrimSpace(raw))
		if rule == "" || rule == "<local>" {
			continue
		}
		if idx := strings.Index(rule, "://"); idx >= 0 {
			rule = rule[idx+3:]
		}
		if idx := strings.IndexByte(rule, '/'); idx >= 0 {
			rule = rule[:idx]
		}
		// Strip leading wildcard for standard no_proxy suffix format (e.g. *.local -> .local, *zhihu.com -> zhihu.com).
		rule = strings.TrimPrefix(rule, "*")
		// Skip rules that still contain wildcards (such as 127.* or 172.2*), which standard no_proxy parsers ignore.
		if rule == "" || strings.ContainsAny(rule, "*?") {
			continue
		}
		if !seen[rule] {
			seen[rule] = true
			entries = append(entries, rule)
		}
	}
	return strings.Join(entries, ",")
}
