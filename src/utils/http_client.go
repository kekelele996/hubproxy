package utils

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"hubproxy/config"
)

// managedClients 一组按当前代理配置构建的 HTTP 客户端
type managedClients struct {
	proxy  string
	global *http.Client
	search *http.Client
}

var clientsHolder atomic.Pointer[managedClients]

// dynamicGlobalTransport 始终委托给当前全局客户端的 Transport。
// 启动时持有它的组件（Docker 代理、镜像下载器）无需重建，
// 代理热更新后新请求自动走新连接，在途请求继续使用旧连接直到完成。
type dynamicGlobalTransport struct{}

func (dynamicGlobalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return GetGlobalHTTPClient().Transport.RoundTrip(req)
}

// GlobalTransport 跟随代理配置热更新的稳定 Transport
var GlobalTransport http.RoundTripper = dynamicGlobalTransport{}

// parseProxyURL 解析上游代理地址，留空表示直连（返回 nil）
func parseProxyURL(proxy string) (*url.URL, error) {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		return nil, nil
	}

	u, err := url.Parse(proxy)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("无效的代理地址 %q", proxy)
	}

	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（仅支持 http/https/socks5）", u.Scheme)
	}

	return u, nil
}

// buildHTTPClients 按配置中的代理地址构建 HTTP 客户端
func buildHTTPClients(cfg *config.AppConfig) *managedClients {
	proxyFunc := func(*http.Request) (*url.URL, error) { return nil, nil } // 默认直连

	if u, err := parseProxyURL(cfg.Access.Proxy); err != nil {
		fmt.Printf("警告: %v，本次将直连上游\n", err)
	} else if u != nil {
		proxyFunc = http.ProxyURL(u)
	}

	globalHTTPClient := &http.Client{
		Transport: &http.Transport{
			Proxy: proxyFunc,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          1000,
			MaxIdleConnsPerHost:   1000,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 300 * time.Second,
		},
	}

	searchHTTPClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: proxyFunc,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			DisableCompression:  false,
		},
	}

	return &managedClients{
		proxy:  strings.TrimSpace(cfg.Access.Proxy),
		global: globalHTTPClient,
		search: searchHTTPClient,
	}
}

// InitHTTPClients 初始化HTTP客户端，并在配置热加载时按需平滑切换代理
func InitHTTPClients() {
	clientsHolder.Store(buildHTTPClients(config.GetConfig()))

	config.OnReload(func(cfg *config.AppConfig) {
		proxy := strings.TrimSpace(cfg.Access.Proxy)
		if current := clientsHolder.Load(); current != nil && current.proxy == proxy {
			return
		}

		old := clientsHolder.Swap(buildHTTPClients(cfg))
		if old != nil {
			// 只关闭空闲连接；已经在途的请求继续持有旧 Transport 完成本次请求
			if t, ok := old.global.Transport.(*http.Transport); ok {
				t.CloseIdleConnections()
			}
			if t, ok := old.search.Transport.(*http.Transport); ok {
				t.CloseIdleConnections()
			}
		}

		if proxy == "" {
			fmt.Printf("上游代理配置已更新: 已切换为直连\n")
		} else {
			fmt.Printf("上游代理配置已更新: %s\n", proxy)
		}
	})
}

// GetGlobalHTTPClient 获取当前HTTP客户端（代理热更新后自动返回新实例）
func GetGlobalHTTPClient() *http.Client {
	return clientsHolder.Load().global
}

// GetSearchHTTPClient 获取搜索HTTP客户端（代理热更新后自动返回新实例）
func GetSearchHTTPClient() *http.Client {
	return clientsHolder.Load().search
}
