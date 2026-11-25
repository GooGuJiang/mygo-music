//go:build !windows

package sysproxy

import (
	"net/http"
	"net/url"
)

func systemProxy(*http.Request) (*url.URL, error) { return nil, nil }

func systemProxyEnv() []string { return nil }
