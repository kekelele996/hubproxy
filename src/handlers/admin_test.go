package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/utils"
)

func setupAdminRouter(t *testing.T, configBody string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	utils.InitAccessLogger(true, 100)
	base := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	utils.GlobalAccessLogger.Add(utils.AccessRecord{
		Time:       base,
		IP:         "10.0.0.1",
		Method:     http.MethodGet,
		Path:       "/v2/library/nginx/blobs/sha256:x",
		Type:       utils.AccessTypeDocker,
		Target:     "library/nginx",
		StatusCode: http.StatusOK,
		BytesOut:   1024,
	})
	utils.GlobalAccessLogger.Add(utils.AccessRecord{
		Time:       base.Add(time.Hour),
		IP:         "10.0.0.2",
		Method:     http.MethodGet,
		Path:       "/https://github.com/a/b/raw/main/f.zip",
		Type:       utils.AccessTypeGitHub,
		Target:     "https://github.com/a/b/raw/main/f.zip",
		StatusCode: http.StatusBadGateway,
		BytesOut:   2048,
	})

	router := gin.New()
	RegisterAdminRoutes(router)
	return router
}

func adminRequest(router *gin.Engine, remoteAddr, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/access/summary?group_by=ip", nil)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestAdminSummaryRequiresConfiguredToken(t *testing.T) {
	router := setupAdminRouter(t, `
[admin]
token = "secret-token"
`)

	// 无令牌 -> 401
	if w := adminRequest(router, "10.2.3.4:1000", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401; body=%s", w.Code, w.Body.String())
	}

	// 错误令牌 -> 401
	if w := adminRequest(router, "10.2.3.4:1000", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", w.Code)
	}

	// 正确令牌 -> 200
	w := adminRequest(router, "10.2.3.4:1000", "secret-token")
	if w.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		GroupBy string `json:"group_by"`
		Totals  struct {
			Requests int64 `json:"requests"`
			BytesOut int64 `json:"bytes_out"`
			Errors   int64 `json:"errors"`
		} `json:"totals"`
		Groups []struct {
			Key      string `json:"key"`
			Requests int64  `json:"requests"`
			BytesOut int64  `json:"bytes_out"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.GroupBy != "ip" || resp.Totals.Requests != 2 || resp.Totals.BytesOut != 3072 || resp.Totals.Errors != 1 {
		t.Fatalf("unexpected summary payload: %s", w.Body.String())
	}
	if len(resp.Groups) != 2 || resp.Groups[0].Key != "10.0.0.2" {
		t.Fatalf("unexpected groups: %s", w.Body.String())
	}
}

func TestAdminWithoutTokenAllowsLoopbackOnly(t *testing.T) {
	router := setupAdminRouter(t, "")

	// 非本机 -> 403
	if w := adminRequest(router, "10.2.3.4:1000", ""); w.Code != http.StatusForbidden {
		t.Fatalf("remote status = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	// 本机回环 -> 200
	if w := adminRequest(router, "127.0.0.1:1000", ""); w.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminSummaryRejectsInvalidGroupBy(t *testing.T) {
	router := setupAdminRouter(t, `
[admin]
token = "secret"
`)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/access/summary?group_by=bogus", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestAdminLogsFilterAndPagination(t *testing.T) {
	router := setupAdminRouter(t, `
[admin]
token = "secret"
`)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/access/logs?type=docker&page=1&page_size=10", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Total int                  `json:"total"`
		Items []utils.AccessRecord `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("docker filter total=%d items=%d", resp.Total, len(resp.Items))
	}
	if resp.Items[0].Target != "library/nginx" || resp.Items[0].BytesOut != 1024 {
		t.Fatalf("unexpected item: %+v", resp.Items[0])
	}
}

func TestAdminTokenFromEnv(t *testing.T) {
	router := setupAdminRouter(t, "")
	t.Setenv("ADMIN_TOKEN", "env-secret")
	// 重新加载配置以应用环境变量
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	if w := adminRequest(router, "10.2.3.4:1000", "env-secret"); w.Code != http.StatusOK {
		t.Fatalf("env token status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
