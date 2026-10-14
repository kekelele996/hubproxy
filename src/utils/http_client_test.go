package utils

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"hubproxy/config"
)

func reloadHTTPClientConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestParseProxyURL(t *testing.T) {
	if u, err := parseProxyURL(""); err != nil || u != nil {
		t.Fatalf("空代理应直连, u=%v err=%v", u, err)
	}
	if u, err := parseProxyURL("socks5://user:pass@127.0.0.1:1080"); err != nil || u == nil || u.Host != "127.0.0.1:1080" {
		t.Fatalf("socks5 代理解析失败: u=%v err=%v", u, err)
	}
	if _, err := parseProxyURL("not-a-proxy"); err == nil {
		t.Fatal("非法代理地址应返回错误")
	}
	if _, err := parseProxyURL("ftp://127.0.0.1:21"); err == nil {
		t.Fatal("不支持的代理协议应返回错误")
	}
}

func TestHTTPClientsHotReloadProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	reloadHTTPClientConfig(t, `
[access]
proxy = ""
`)
	InitHTTPClients()

	roundTrip := func() error {
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := GlobalTransport.RoundTrip(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	if err := roundTrip(); err != nil {
		t.Fatalf("直连应当成功: %v", err)
	}
	directClient := GetGlobalHTTPClient()

	// 热更新为一个不可用的代理：新请求立即走代理并失败，说明新连接即时生效
	reloadHTTPClientConfig(t, `
[access]
proxy = "http://127.0.0.1:1"
`)
	if err := config.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if GetGlobalHTTPClient() == directClient {
		t.Fatal("代理变更后应当切换到新的 HTTP 客户端")
	}
	if err := roundTrip(); err == nil {
		t.Fatal("代理热更新后请求应经过新代理（此处预期连接失败）")
	}

	// 热更新恢复直连
	reloadHTTPClientConfig(t, `
[access]
proxy = ""
`)
	if err := config.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(); err != nil {
		t.Fatalf("恢复直连后应当成功: %v", err)
	}
}
