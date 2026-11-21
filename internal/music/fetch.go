package music

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Agent is who the app says it is when fetching what it plays and shows.
const Agent = "MyGoMusic/1.0"

// fetchClient gets covers: they're small, and a slow one shouldn't hold a
// slot for long.
var fetchClient = &http.Client{Timeout: 30 * time.Second}

// ErrImageTemporary marks a cover that can be fetched again after a network failure.
var ErrImageTemporary = errors.New("temporary image fetch failure")

// maxFetch is the most a cover may take.
const maxFetch = 64 << 20

// ImageHeaders, when set, answers the headers to fetch the picture at u with,
// such as the cookie of a source that keeps its covers behind its session.
// It may answer nil.
var ImageHeaders func(u *url.URL) map[string]string

// SizedImage returns the URL to fetch the picture at u with to show it px
// pixels across: u with its size filled in, where it says it goes with
// {size}, else u itself.
func SizedImage(u string, px int) string {
	if px <= 0 {
		px = placeholderSize
	}
	if !strings.Contains(u, "size") {
		return u
	}
	return sizePlaceholder.ReplaceAllLiteralString(u, strconv.Itoa(px))
}

// placeholderSize is the size a picture whose URL says where its size goes
// is fetched at where nothing asks for one: the most the app shows one at.
const placeholderSize = 600

// sizePlaceholder is where a picture's URL says its size goes, in the URL
// as a source gives it or escaped, as a URL parsed and written again has it.
var sizePlaceholder = regexp.MustCompile(`\{size\}|%7[Bb]size%7[Dd]`)

// Fetch returns what's at url, of the web or a file://, such as a cover.
func Fetch(u string) ([]byte, error) {
	return FetchContext(context.Background(), u)
}

// FetchContext fetches a cover and stops waiting when ctx is canceled.
func FetchContext(ctx context.Context, u string) ([]byte, error) {
	if path, ok := PathOf(u); ok {
		return os.ReadFile(path)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Host == "" || req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, errors.New("invalid cover URL")
	}
	req.Header.Set("User-Agent", Agent)
	var headers map[string]string
	if ImageHeaders != nil {
		headers = ImageHeaders(req.URL)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := HeadersStay(fetchClient, req.URL.Hostname(), headers)
	data, err := fetchImage(client, req)
	if errors.Is(err, ErrImageTemporary) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			data, err = fetchImage(client, req)
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return data, err
}

func fetchImage(client *http.Client, req *http.Request) ([]byte, error) {
	res, err := DoWithIdleTimeout(client, req, 7*time.Second)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageTemporary, err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return nil, fmt.Errorf("%w: %s", ErrImageTemporary, res.Status)
	default:
		return nil, errors.New(res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxFetch+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageTemporary, err)
	}
	if len(data) > maxFetch {
		return nil, errors.New("image response is too large")
	}
	return data, nil
}

// ErrTransferIdle signals that an HTTP request stopped receiving data.
var ErrTransferIdle = errors.New("HTTP transfer stopped making progress")

// DoWithIdleTimeout bounds the wait for headers and for each body read.
// The caller must close the response body.
func DoWithIdleTimeout(client *http.Client, req *http.Request, idle time.Duration) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(idle, func() { cancel(ErrTransferIdle) })
	res, err := client.Do(req.WithContext(ctx))
	if !timer.Stop() {
		cancel(ErrTransferIdle)
	}
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if err != nil {
		cancel(nil)
		if res != nil {
			res.Body.Close()
		}
		return nil, err
	}
	res.Body = &idleBody{ReadCloser: res.Body, ctx: ctx, cancel: cancel, idle: idle}
	return res, nil
}

type idleBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	idle   time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	timer := time.AfterFunc(b.idle, func() { b.cancel(ErrTransferIdle) })
	n, err := b.ReadCloser.Read(p)
	if !timer.Stop() {
		b.cancel(ErrTransferIdle)
	}
	if cause := context.Cause(b.ctx); cause != nil {
		return n, cause
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.cancel(nil)
	return b.ReadCloser.Close()
}

// HeadersStay returns client, or, with headers to send, one like it that
// asks a redirect to a host other than host, or one below it, without them,
// so that what a source asks with, such as its token, reaches no one else.
// It's the rule Go keeps for Cookie and Authorization, for every header.
func HeadersStay(client *http.Client, host string, headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return client
	}
	host = strings.ToLower(host)
	c := *client
	c.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if to := strings.ToLower(next.URL.Hostname()); to != host && !strings.HasSuffix(to, "."+host) {
			for k := range headers {
				next.Header.Del(k)
			}
		}
		return nil
	}
	return &c
}

// FileURL is the file:// URL of a path.
func FileURL(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path // C:/Music
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// PathOf returns the path a file:// URL names, and whether u is one.
func PathOf(u string) (string, bool) {
	if !strings.HasPrefix(u, "file:") {
		return "", false
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Path == "" {
		return "", false
	}
	path := parsed.Path
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, "/")
	}
	return filepath.FromSlash(path), true
}
