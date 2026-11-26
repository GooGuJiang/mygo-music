package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"

	"mygo-music/internal/plugin"
)

const (
	webFetchReadyTimeout  = 15 * time.Second
	webFetchCallTimeout   = 30 * time.Second
	webFetchVerifyTimeout = 4 * time.Minute
)

// webFetchHost keeps reusable per-plugin WebView pages on their site origin so
// plugins can execute browser-backed requests (with real cookies & TLS) and
// show a visible verification window when the site asks for a human check.
type webFetchHost struct {
	app      *app
	mu       sync.Mutex
	browsers map[string]*webFetchBrowser
	closing  bool
}

type webFetchBrowser struct {
	mu      sync.Mutex
	win     *mygo.Window
	session *mygo.Session
	origin  string

	verifyMu     sync.Mutex
	verifyCancel context.CancelFunc
	closed       bool
}

func newWebFetchHost(a *app) *webFetchHost {
	return &webFetchHost{
		app:      a,
		browsers: map[string]*webFetchBrowser{},
	}
}

func (h *webFetchHost) closeAll() {
	h.mu.Lock()
	h.closing = true
	browsers := make([]*webFetchBrowser, 0, len(h.browsers))
	for _, b := range h.browsers {
		browsers = append(browsers, b)
	}
	h.browsers = map[string]*webFetchBrowser{}
	h.mu.Unlock()

	for _, b := range browsers {
		b.destroy()
	}
}

func (b *webFetchBrowser) destroy() {
	b.verifyMu.Lock()
	b.closed = true
	cancel := b.verifyCancel
	b.verifyCancel = nil
	b.verifyMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if b.win != nil && !b.win.IsDestroyed() {
		b.win.Destroy()
	}
}

func (h *webFetchHost) getBrowser(req plugin.WebFetchRequest) (*webFetchBrowser, error) {
	var available bool
	mygo.RunOnMain(func() { available = mygo.WebViewAvailable() })
	if !available {
		return nil, errors.New(T("This computer can't show the verification page"))
	}

	key := req.Plugin + "\x00" + req.Origin
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, errors.New("app is shutting down")
	}
	if b := h.browsers[key]; b != nil && !b.win.IsDestroyed() {
		h.mu.Unlock()
		return b, nil
	}
	h.mu.Unlock()

	session := mygo.NewSession("plugin-" + req.Plugin)
	ready := make(chan struct{})
	var readyOnce sync.Once
	markReady := func() { readyOnce.Do(func() { close(ready) }) }

	b := &webFetchBrowser{
		session: session,
		origin:  req.Origin,
	}

	win := mygo.NewWindow(mygo.WindowOptions{
		Title:     Tf("Verify %s", req.Name),
		URL:       req.Origin,
		Width:     480,
		Height:    640,
		MinWidth:  360,
		MinHeight: 480,
		Hidden:    true,
		Parent:    h.app.win,
		Page: mygo.PageOptions{
			Session:              session,
			UserAgent:            req.UserAgent,
			BackgroundThrottling: mygo.ThrottlingOff,
		},
	})
	b.win = win
	win.Page().OnDOMReady(markReady)
	win.Page().OnDidFinishLoad(markReady)
	win.OnClose(func(e *mygo.CloseEvent) {
		b.verifyMu.Lock()
		closing := b.closed || h.app.quitting
		cancel := b.verifyCancel
		b.verifyMu.Unlock()
		if closing {
			return
		}
		// User closed the verification popup: cancel pending check and keep
		// the hidden WebView reusable for later requests.
		e.PreventDefault()
		if cancel != nil {
			cancel()
		}
		win.Hide()
	})

	select {
	case <-ready:
	case <-time.After(webFetchReadyTimeout):
		b.destroy()
		return nil, errors.New("timed out preparing the browser window")
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		b.destroy()
		return nil, errors.New("app is shutting down")
	}
	if existing := h.browsers[key]; existing != nil && !existing.win.IsDestroyed() {
		h.mu.Unlock()
		b.destroy()
		return existing, nil
	}
	h.browsers[key] = b
	h.mu.Unlock()
	return b, nil
}

func (h *webFetchHost) do(req plugin.WebFetchRequest) (plugin.WebFetchResponse, error) {
	b, err := h.getBrowser(req)
	if err != nil {
		return plugin.WebFetchResponse{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.win.IsDestroyed() {
		return plugin.WebFetchResponse{}, errors.New("browser window closed")
	}

	res, err := b.evalFetch(req)
	if err != nil {
		return plugin.WebFetchResponse{}, err
	}

	captchaURL := extractCaptchaURL(res)
	if captchaURL == "" {
		return res, nil
	}

	// Show the real browser window so the listener can complete verification
	// directly inside an iframe hosted on the site's own origin.
	verified, verifyErr := b.promptInteractiveVerification(req.Name, captchaURL)
	if verifyErr != nil || !verified {
		return res, nil
	}

	return b.evalFetch(req)
}

func (b *webFetchBrowser) evalFetch(req plugin.WebFetchRequest) (plugin.WebFetchResponse, error) {
	payload, err := json.Marshal(struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Headers map[string]string `json:"headers,omitempty"`
		Body    string            `json:"body,omitempty"`
		HasBody bool              `json:"hasBody"`
	}{
		URL:     req.URL,
		Method:  req.Method,
		Headers: req.Headers,
		Body:    req.Body,
		HasBody: req.Body != "",
	})
	if err != nil {
		return plugin.WebFetchResponse{}, err
	}

	script := fmt.Sprintf(`
const opts = %s;
const init = {
  method: opts.method || "GET",
  headers: opts.headers || {},
  credentials: "include",
  mode: "cors",
};
if (opts.hasBody) init.body = opts.body;
const resp = await fetch(opts.url, init);
const text = await resp.text();
return { status: resp.status, text };
`, string(payload))

	ctx, cancel := context.WithTimeout(context.Background(), webFetchCallTimeout)
	defer cancel()

	raw, err := b.win.Page().EvalContext(ctx, script)
	if err != nil {
		return plugin.WebFetchResponse{}, err
	}
	return decodeWebFetchResponse(raw)
}

func decodeWebFetchResponse(raw any) (plugin.WebFetchResponse, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return plugin.WebFetchResponse{}, errors.New("unexpected browser fetch response")
	}
	status := 0
	if v, ok := m["status"].(float64); ok {
		status = int(v)
	}
	text, _ := m["text"].(string)
	return plugin.WebFetchResponse{Status: status, Text: text}, nil
}

// extractCaptchaURL returns the DataDome verification URL when the site answers
// 403 with a captcha-delivery.com JSON payload, or "" otherwise.
func extractCaptchaURL(res plugin.WebFetchResponse) string {
	if res.Status != 403 || res.Text == "" {
		return ""
	}
	var payload struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil || payload.URL == "" {
		return ""
	}
	u, err := url.Parse(payload.URL)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "captcha-delivery.com" && !strings.HasSuffix(host, ".captcha-delivery.com") {
		return ""
	}
	return payload.URL
}

func (b *webFetchBrowser) promptInteractiveVerification(name, captchaURL string) (bool, error) {
	encodedURL, err := json.Marshal(captchaURL)
	if err != nil {
		return false, err
	}

	verifyCtx, cancel := context.WithTimeout(context.Background(), webFetchVerifyTimeout)
	b.verifyMu.Lock()
	b.verifyCancel = cancel
	b.verifyMu.Unlock()

	defer func() {
		b.verifyMu.Lock()
		b.verifyCancel = nil
		b.verifyMu.Unlock()
		cancel()
		if !b.win.IsDestroyed() {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = b.win.Page().EvalContext(cleanCtx, `const el = document.getElementById("__mygo_verify_overlay"); if (el) el.remove();`)
			cleanCancel()
			b.win.Hide()
		}
	}()

	b.win.SetTitle(Tf("Verify %s", name))
	b.win.Center()
	b.win.Show()
	b.win.Focus()

	// Host the site's verification URL inside an iframe on the site's own origin
	// so its postMessage({ cookie: "datadome=..." }) writes the cookie onto the
	// site's domain and closes the window immediately once the user completes it.
	script := fmt.Sprintf(`
const captchaUrl = %s;
return await new Promise((resolve) => {
  const existing = document.getElementById("__mygo_verify_overlay");
  if (existing) existing.remove();

  const overlay = document.createElement("div");
  overlay.id = "__mygo_verify_overlay";
  overlay.style.cssText = "position:fixed;inset:0;z-index:2147483647;background:#ffffff;margin:0;padding:0;";

  const frame = document.createElement("iframe");
  frame.src = captchaUrl;
  frame.style.cssText = "width:100%%;height:100%%;border:0;display:block;";
  frame.setAttribute("allow", "autoplay");
  overlay.appendChild(frame);
  document.body.appendChild(overlay);

  let settled = false;
  const finish = (ok) => {
    if (settled) return;
    settled = true;
    window.removeEventListener("message", onMessage);
    window.removeEventListener("dd_response_passed", onPassed);
    overlay.remove();
    resolve(ok);
  };

  const onPassed = () => finish(true);

  const onMessage = (event) => {
    let data = event.data;
    if (typeof data === "string") {
      try { data = JSON.parse(data); } catch { return; }
    }
    if (!data || typeof data !== "object") return;
    if (typeof data.cookie === "string" && data.cookie.includes("datadome=")) {
      try { document.cookie = data.cookie; } catch {}
      finish(true);
    }
  };

  window.addEventListener("message", onMessage);
  window.addEventListener("dd_response_passed", onPassed);
});
`, string(encodedURL))

	raw, err := b.win.Page().EvalContext(verifyCtx, script)
	if err != nil {
		return false, err
	}
	ok, _ := raw.(bool)
	return ok, nil
}
