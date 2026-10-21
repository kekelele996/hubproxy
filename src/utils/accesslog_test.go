package utils

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newAccessRecord(ip, recType, target string, status int, bytesOut int64, at time.Time) AccessRecord {
	return AccessRecord{
		Time:       at,
		IP:         ip,
		Method:     http.MethodGet,
		Path:       "/v2/" + target + "/blobs/sha256:abc",
		Type:       recType,
		Target:     target,
		StatusCode: status,
		BytesOut:   bytesOut,
	}
}

func TestAccessLogStoreRingEvictionAndOrder(t *testing.T) {
	store := NewAccessLogStore(true, 3)
	base := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 4; i++ {
		store.Add(newAccessRecord("10.0.0.1", AccessTypeDocker, "library/nginx", 200, 10, base.Add(time.Duration(i)*time.Minute)))
	}

	items, total := store.Query(AccessLogFilter{}, 1, 10)
	if total != 3 {
		t.Fatalf("total = %d, want 3 (capacity)", total)
	}
	if len(items) != 3 {
		t.Fatalf("len(items) = %d, want 3", len(items))
	}

	// 最新记录在前，最旧记录（i=0）已被淘汰
	if items[0].Target != "library/nginx" || !items[0].Time.Equal(base.Add(3*time.Minute)) {
		t.Fatalf("newest record mismatch: %+v", items[0])
	}
	if !items[2].Time.Equal(base.Add(1 * time.Minute)) {
		t.Fatalf("oldest retained record mismatch: %+v", items[2])
	}
	if items[0].ID <= items[1].ID {
		t.Fatalf("ID should monotonically increase even after eviction: %d %d", items[0].ID, items[1].ID)
	}
}

func TestAccessLogStoreQueryFilterAndPagination(t *testing.T) {
	store := NewAccessLogStore(true, 100)
	base := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

	store.Add(newAccessRecord("10.0.0.1", AccessTypeDocker, "library/nginx", 200, 100, base))
	store.Add(newAccessRecord("10.0.0.2", AccessTypeGitHub, "https://github.com/a/b/raw/main/x", 200, 200, base.Add(time.Minute)))
	store.Add(newAccessRecord("10.0.0.1", AccessTypeDocker, "library/redis", 500, 300, base.Add(2*time.Minute)))

	// 按 IP 过滤
	items, total := store.Query(AccessLogFilter{IP: "10.0.0.1"}, 1, 10)
	if total != 2 || len(items) != 2 {
		t.Fatalf("ip filter total=%d len=%d, want 2/2", total, len(items))
	}

	// 按类型 + 关键字过滤
	items, total = store.Query(AccessLogFilter{Type: AccessTypeGitHub, Target: "github.com/a/b"}, 1, 10)
	if total != 1 || items[0].IP != "10.0.0.2" {
		t.Fatalf("type/target filter mismatch: total=%d items=%+v", total, items)
	}

	// 按状态码过滤
	_, total = store.Query(AccessLogFilter{Status: 500}, 1, 10)
	if total != 1 {
		t.Fatalf("status filter total = %d, want 1", total)
	}

	// 时间范围过滤
	_, total = store.Query(AccessLogFilter{
		StartTime: base.Add(90 * time.Second),
		EndTime:   base.Add(3 * time.Minute),
	}, 1, 10)
	if total != 1 {
		t.Fatalf("time range total = %d, want 1", total)
	}

	// 分页
	page1, total := store.Query(AccessLogFilter{}, 1, 2)
	if total != 3 || len(page1) != 2 {
		t.Fatalf("page1 total=%d len=%d", total, len(page1))
	}
	page2, _ := store.Query(AccessLogFilter{}, 2, 2)
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Fatal("pagination returned overlapping records")
	}

	// 超出范围的页码
	outOfRange, _ := store.Query(AccessLogFilter{}, 9, 2)
	if len(outOfRange) != 0 {
		t.Fatalf("out-of-range page should be empty, got %d", len(outOfRange))
	}
}

func TestAccessLogSummary(t *testing.T) {
	store := NewAccessLogStore(true, 100)
	base := time.Date(2026, 10, 14, 10, 30, 0, 0, time.UTC)

	store.Add(newAccessRecord("10.0.0.1", AccessTypeDocker, "library/nginx", 200, 100, base))
	store.Add(newAccessRecord("10.0.0.1", AccessTypeDocker, "library/redis", 404, 50, base.Add(14*time.Hour)))
	store.Add(newAccessRecord("10.0.0.2", AccessTypeDocker, "library/nginx", 200, 300, base.Add(26*time.Hour)))

	groups, totals := store.Summary("ip", AccessLogFilter{})
	if totals.Requests != 3 || totals.BytesOut != 450 || totals.Errors != 1 {
		t.Fatalf("totals mismatch: %+v", totals)
	}
	if len(groups) != 2 {
		t.Fatalf("groups len = %d, want 2", len(groups))
	}
	// 按流量倒序：10.0.0.2 (300) 在前
	if groups[0].Key != "10.0.0.2" || groups[0].BytesOut != 300 {
		t.Fatalf("top group mismatch: %+v", groups[0])
	}
	if groups[1].Key != "10.0.0.1" || groups[1].Requests != 2 || groups[1].Errors != 1 {
		t.Fatalf("second group mismatch: %+v", groups[1])
	}

	// 按天分组：三条记录跨 10-14 / 10-15
	dayGroups, _ := store.Summary("day", AccessLogFilter{})
	if len(dayGroups) != 2 {
		t.Fatalf("day groups len = %d, want 2", len(dayGroups))
	}

	// 按目标分组
	targetGroups, _ := store.Summary("target", AccessLogFilter{})
	if len(targetGroups) != 2 || targetGroups[0].Key != "library/nginx" || targetGroups[0].Requests != 2 {
		t.Fatalf("target groups mismatch: %+v", targetGroups)
	}

	// 过滤后汇总
	_, totals = store.Summary("ip", AccessLogFilter{IP: "10.0.0.1"})
	if totals.Requests != 2 || totals.BytesOut != 150 {
		t.Fatalf("filtered totals mismatch: %+v", totals)
	}
}

func TestParseDockerTargetFromPath(t *testing.T) {
	cases := map[string]string{
		"library/nginx/manifests/latest":  "library/nginx",
		"nginx/blobs/sha256:abc":          "library/nginx",
		"ghcr.io/owner/repo/manifests/v1": "ghcr.io/owner/repo",
		"library/nginx/tags/list":         "library/nginx",
		"":                                "",
		"library/nginx/unknown/latest":    "",
	}
	for path, want := range cases {
		if got := parseDockerTargetFromPath(path); got != want {
			t.Fatalf("parseDockerTargetFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestTruncateString(t *testing.T) {
	if got := truncateString("short", 10); got != "short" {
		t.Fatalf("short string altered: %q", got)
	}
	got := truncateString("0123456789", 5)
	if got != "01234..." {
		t.Fatalf("truncate = %q", got)
	}
}

func TestAccessLogMiddlewareRecordsAccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccessLogStore(true, 100)

	router := gin.New()
	router.Use(AccessLogMiddleware(store))
	router.GET("/v2/*path", func(c *gin.Context) {
		c.Set(ContextKeyAccessType, AccessTypeDocker)
		c.Set(ContextKeyAccessTarget, "library/nginx")
		c.Data(http.StatusOK, "application/octet-stream", []byte("hello-bytes")) // 11 字节
	})
	router.GET("/ready", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/blobs/sha256:x", nil)
	req.RemoteAddr = "10.1.2.3:54321"
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	items, total := store.Query(AccessLogFilter{}, 1, 10)
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	rec := items[0]
	if rec.IP != "10.1.2.3" {
		t.Fatalf("ip = %q", rec.IP)
	}
	if rec.Type != AccessTypeDocker || rec.Target != "library/nginx" {
		t.Fatalf("resource = %q %q", rec.Type, rec.Target)
	}
	if rec.StatusCode != http.StatusOK || rec.BytesOut != 11 {
		t.Fatalf("status=%d bytesOut=%d", rec.StatusCode, rec.BytesOut)
	}
	if rec.DurationMs < 0 {
		t.Fatalf("duration = %d", rec.DurationMs)
	}

	// /ready 属于跳过路径，不产生记录
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/ready", nil))
	_, total = store.Query(AccessLogFilter{}, 1, 10)
	if total != 1 {
		t.Fatalf("skipped path recorded, total = %d", total)
	}
}

func TestAccessLogMiddlewareDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccessLogStore(false, 100)

	router := gin.New()
	router.Use(AccessLogMiddleware(store))
	router.GET("/v2/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/x", nil))

	_, total := store.Query(AccessLogFilter{}, 1, 10)
	if total != 0 {
		t.Fatalf("disabled logger recorded %d records", total)
	}
}

func TestAccessLogMiddlewareFallbackClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewAccessLogStore(true, 100)

	router := gin.New()
	router.Use(AccessLogMiddleware(store))
	// handler 不标注资源，验证中间件按路径兜底解析
	router.GET("/v2/*path", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/nginx/manifests/latest", nil))

	items, _ := store.Query(AccessLogFilter{}, 1, 10)
	if len(items) != 1 {
		t.Fatalf("records = %d, want 1", len(items))
	}
	if items[0].Type != AccessTypeDocker || items[0].Target != "library/nginx" {
		t.Fatalf("fallback classify = %q %q", items[0].Type, items[0].Target)
	}
}
