package sysproxy

import (
	"net/http"
	"net/url"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	configCacheTTL      = 2 * time.Second

	winhttpAccessTypeNoProxy    = 1
	winhttpAccessTypeNamedProxy = 3
	winhttpAutoProxyConfigURL   = 0x00000002
)

var (
	cacheMu     sync.Mutex
	cachedCfg   config
	cachedAt    time.Time
	hasCached   bool
	winhttpDLL  = windows.NewLazySystemDLL("winhttp.dll")
	kernel32DLL = windows.NewLazySystemDLL("kernel32.dll")

	procWinHttpOpen           = winhttpDLL.NewProc("WinHttpOpen")
	procWinHttpCloseHandle    = winhttpDLL.NewProc("WinHttpCloseHandle")
	procWinHttpGetProxyForUrl = winhttpDLL.NewProc("WinHttpGetProxyForUrl")
	procGlobalFree            = kernel32DLL.NewProc("GlobalFree")
)

type winhttpAutoProxyOptions struct {
	Flags                 uint32
	AutoDetectFlags       uint32
	AutoConfigUrl         *uint16
	Reserved              uintptr
	ReservedLen           uint32
	AutoLogonIfChallenged int32
}

type winhttpProxyInfo struct {
	AccessType  uint32
	Proxy       *uint16
	ProxyBypass *uint16
}

func readWindowsConfig() config {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if hasCached && time.Since(cachedAt) < configCacheTTL {
		return cachedCfg
	}
	cfg := loadWindowsRegistryConfig()
	cachedCfg = cfg
	cachedAt = time.Now()
	hasCached = true
	return cfg
}

func loadWindowsRegistryConfig() config {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return config{}
	}
	defer k.Close()

	var cfg config
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && v != 0 {
		cfg.Enabled = true
	}
	if v, _, err := k.GetStringValue("ProxyServer"); err == nil {
		cfg.Server = v
	}
	if v, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		cfg.Override = v
	}
	if v, _, err := k.GetStringValue("AutoConfigURL"); err == nil {
		cfg.AutoConfigURL = v
	}
	return cfg
}

func systemProxy(req *http.Request) (*url.URL, error) {
	cfg := readWindowsConfig()
	if cfg.Enabled {
		return resolveConfig(cfg, req)
	}
	if cfg.AutoConfigURL != "" {
		return pacProxy(cfg.AutoConfigURL, req.URL)
	}
	return nil, nil
}

func systemProxyEnv() []string {
	cfg := readWindowsConfig()
	if cfg.Enabled {
		return envFromConfig(cfg)
	}
	if cfg.AutoConfigURL != "" {
		probe, _ := url.Parse("https://soundcloud.com/")
		if u, err := pacProxy(cfg.AutoConfigURL, probe); err == nil && u != nil {
			return envFromProxyURLs(u, u, cfg.Override)
		}
	}
	return nil
}

func pacProxy(pacURL string, target *url.URL) (*url.URL, error) {
	if target == nil || pacURL == "" {
		return nil, nil
	}
	if err := winhttpDLL.Load(); err != nil {
		return nil, nil
	}
	agent, err := windows.UTF16PtrFromString("MyGoMusic/1.0")
	if err != nil {
		return nil, nil
	}
	session, _, _ := procWinHttpOpen.Call(
		uintptr(unsafe.Pointer(agent)),
		winhttpAccessTypeNoProxy,
		0, 0, 0,
	)
	if session == 0 {
		return nil, nil
	}
	defer procWinHttpCloseHandle.Call(session)

	targetPtr, err := windows.UTF16PtrFromString(target.String())
	if err != nil {
		return nil, nil
	}
	pacPtr, err := windows.UTF16PtrFromString(pacURL)
	if err != nil {
		return nil, nil
	}
	opts := winhttpAutoProxyOptions{
		Flags:                 winhttpAutoProxyConfigURL,
		AutoConfigUrl:         pacPtr,
		AutoLogonIfChallenged: 1,
	}
	var info winhttpProxyInfo
	r1, _, _ := procWinHttpGetProxyForUrl.Call(
		session,
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(unsafe.Pointer(&opts)),
		uintptr(unsafe.Pointer(&info)),
	)
	if info.Proxy != nil {
		defer procGlobalFree.Call(uintptr(unsafe.Pointer(info.Proxy)))
	}
	if info.ProxyBypass != nil {
		defer procGlobalFree.Call(uintptr(unsafe.Pointer(info.ProxyBypass)))
	}
	if r1 == 0 || info.AccessType != winhttpAccessTypeNamedProxy || info.Proxy == nil {
		return nil, nil
	}
	proxyStr := windows.UTF16PtrToString(info.Proxy)
	bypassStr := ""
	if info.ProxyBypass != nil {
		bypassStr = windows.UTF16PtrToString(info.ProxyBypass)
	}
	if shouldBypass(target.Hostname(), target.Port(), bypassStr) {
		return nil, nil
	}
	return parseProxyServer(proxyStr, target.Scheme)
}
