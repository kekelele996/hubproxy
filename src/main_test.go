package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/handlers"
	"hubproxy/utils"
)

func newTestRouter(t *testing.T, configBody string) *gin.Engine {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(configBody), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	utils.InitHTTPClients()
	globalLimiter = utils.InitGlobalLimiter()
	handlers.InitDockerProxy()
	handlers.InitImageStreamer()
	handlers.InitDebouncer()

	return buildRouter(config.GetConfig())
}

func performRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "hubproxy-test")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestReadyRoute(t *testing.T) {
	router := newTestRouter(t, "")

	w := performRequest(router, http.MethodGet, "/ready", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["ready"] != true || got["service"] != "hubproxy" {
		t.Fatalf("unexpected ready response: %#v", got)
	}
}

func TestFrontendDisabledRoutesReturnNotFound(t *testing.T) {
	router := newTestRouter(t, `
[server]
enableFrontend = false
`)

	for _, path := range []string{"/", "/images", "/search", "/favicon.ico"} {
		w := performRequest(router, http.MethodGet, path, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, w.Code)
		}
	}
}

func TestSingleImageDownloadPrepareReturnsURL(t *testing.T) {
	router := newTestRouter(t, "")

	w := performRequest(router, http.MethodGet, "/api/image/download?image=nginx&mode=prepare", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.DownloadURL, "image=nginx") || !strings.Contains(got.DownloadURL, "token=") {
		t.Fatalf("download_url = %q", got.DownloadURL)
	}
	if !strings.HasPrefix(got.DownloadURL, "/api/image/download?") {
		t.Fatalf("download_url = %q", got.DownloadURL)
	}
}

func TestBatchImageDownloadPrepareReturnsURL(t *testing.T) {
	router := newTestRouter(t, "")

	body := `{"images":["nginx"],"useCompressedLayers":true}`
	w := performRequest(router, http.MethodPost, "/api/image/batch?mode=prepare", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var got struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.DownloadURL, "/api/image/batch?token=") {
		t.Fatalf("download_url = %q", got.DownloadURL)
	}
}

func TestBatchImageDownloadRejectsTooManyImages(t *testing.T) {
	router := newTestRouter(t, `
[download]
maxImages = 1
`)

	body := `{"images":["nginx","redis"],"useCompressedLayers":true}`
	w := performRequest(router, http.MethodPost, "/api/image/batch?mode=prepare", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestGitHubNoRouteRejectsUnsupportedHost(t *testing.T) {
	router := newTestRouter(t, "")

	w := performRequest(router, http.MethodGet, "/https://example.com/file.zip", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

func TestDockerV2PingAndInvalidPath(t *testing.T) {
	router := newTestRouter(t, "")

	w := performRequest(router, http.MethodGet, "/v2/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/v2/ status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	w = performRequest(router, http.MethodGet, "/v2/library/nginx/unknown/latest", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid v2 status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestSearchAPIRejectsMissingQuery(t *testing.T) {
	router := newTestRouter(t, `
[server]
enableFrontend = false
`)

	w := performRequest(router, http.MethodGet, "/api/search", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["error"] == "" {
		t.Fatalf("missing error response: %#v", got)
	}
}

// reloadRunningConfig 覆写当前 CONFIG_PATH 指向的配置文件并热加载，不重建 router
func reloadRunningConfig(t *testing.T, body string) {
	t.Helper()
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		t.Fatal("CONFIG_PATH 未设置")
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.ReloadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsHotReloadWithoutRestart(t *testing.T) {
	router := newTestRouter(t, "")

	// 初始：镜像可下载
	w := performRequest(router, http.MethodGet, "/api/image/download?image=alpine&mode=prepare", "")
	if w.Code != http.StatusOK {
		t.Fatalf("初始 alpine 应可下载, status=%d body=%s", w.Code, w.Body.String())
	}

	// 1) 热加载资源黑名单：alpine 立即被拒，其他镜像不受影响
	reloadRunningConfig(t, `
[access]
blackList = ["library/alpine"]
`)
	w = performRequest(router, http.MethodGet, "/api/image/download?image=alpine&mode=prepare", "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "黑名单") {
		t.Fatalf("热加载后 alpine 应被镜像黑名单拒绝, status=%d body=%s", w.Code, w.Body.String())
	}
	w = performRequest(router, http.MethodGet, "/api/image/download?image=nginx&mode=prepare", "")
	if w.Code != http.StatusOK {
		t.Fatalf("不在黑名单的 nginx 应正常, status=%d body=%s", w.Code, w.Body.String())
	}

	// 2) 热加载 IP 黑名单：测试客户端 IP 立即在限流中间件层被拒
	reloadRunningConfig(t, `
[access]
blackList = ["library/alpine"]

[security]
blackList = ["192.0.2.1/32"]
`)
	w = performRequest(router, http.MethodGet, "/api/image/download?image=redis&mode=prepare", "")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "限制访问") {
		t.Fatalf("热加载后测试IP应被IP黑名单拒绝, status=%d body=%s", w.Code, w.Body.String())
	}

	// 3) 清空全部名单后立即恢复（同一个 router、同一个服务进程）
	reloadRunningConfig(t, "")
	w = performRequest(router, http.MethodGet, "/api/image/download?image=busybox&mode=prepare", "")
	if w.Code != http.StatusOK {
		t.Fatalf("名单清空后 busybox 应立即恢复访问, status=%d body=%s", w.Code, w.Body.String())
	}

	// 4) 热加载限流额度：收紧到 1次/小时，新请求立即受 429 约束
	w = performRequest(router, http.MethodGet, "/ready", "")
	if w.Code != http.StatusOK {
		t.Fatalf("额度收紧前 /ready 应正常, status=%d", w.Code)
	}
	reloadRunningConfig(t, `
[rateLimit]
requestLimit = 1
periodHours = 1.0
`)
	gotTooManyRequests := false
	for i := 0; i < 5; i++ {
		if performRequest(router, http.MethodGet, "/ready", "").Code == http.StatusTooManyRequests {
			gotTooManyRequests = true
			break
		}
	}
	if !gotTooManyRequests {
		t.Fatal("限流额度热加载收紧后应出现 429")
	}
}

func TestSearchServesSPAWhenFrontendEnabled(t *testing.T) {
	router := newTestRouter(t, `
[server]
enableFrontend = true
`)

	w := performRequest(router, http.MethodGet, "/search?q=nginx", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %q, want text/html", w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), `<div id="app">`) {
		t.Fatalf("SPA shell missing: %s", w.Body.String())
	}
}
