package handlers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestDownloadDebouncer(t *testing.T) {
	d := NewDownloadDebouncer(time.Minute)
	if !d.ShouldAllow("user", "content") {
		t.Fatal("first request denied")
	}
	if d.ShouldAllow("user", "content") {
		t.Fatal("duplicate request allowed")
	}
	if !d.ShouldAllow("other", "content") {
		t.Fatal("different user denied")
	}
}

func TestTokenStoreCreateConsume(t *testing.T) {
	store := newTokenStore[SingleDownloadRequest]()
	req := SingleDownloadRequest{Image: "nginx:latest", Platform: "linux/amd64", UseCompressedLayers: true}

	token, err := store.create(req, "127.0.0.1", "ua")
	if err != nil {
		t.Fatal(err)
	}

	got, ok := store.consume(token, "127.0.0.1", "ua")
	if !ok {
		t.Fatal("token not consumed")
	}
	if got != req {
		t.Fatalf("request = %#v, want %#v", got, req)
	}
	if _, ok := store.consume(token, "127.0.0.1", "ua"); ok {
		t.Fatal("token consumed twice")
	}
}

func TestTokenStoreRejectsDifferentClient(t *testing.T) {
	store := newTokenStore[SingleDownloadRequest]()
	token, err := store.create(SingleDownloadRequest{Image: "nginx:latest"}, "127.0.0.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.consume(token, "127.0.0.2", "ua"); ok {
		t.Fatal("token accepted for different IP")
	}
}

func TestGenerateContentFingerprintStable(t *testing.T) {
	a := generateContentFingerprint([]string{"b:1", "a:1"}, "linux/amd64")
	b := generateContentFingerprint([]string{"a:1", "b:1"}, "linux/amd64")
	c := generateContentFingerprint([]string{"a:1", "b:1"}, "linux/arm64")
	if a != b || a == c {
		t.Fatalf("unexpected fingerprints: %q %q %q", a, b, c)
	}
}

func TestResolveImageRef(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("query preserves underscores", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/api/image/download?image=user/my_app:v1", nil)
		if got := resolveImageRef(c); got != "user/my_app:v1" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("missing image is empty", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/api/image/download", nil)
		if got := resolveImageRef(c); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestWriteDownloadErrorSkipsJSONAfterBodyStarted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("before write returns json", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		writeDownloadError(c, errors.New("boom"), "镜像下载失败")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "镜像下载失败") || !strings.Contains(body, "boom") {
			t.Fatalf("body = %q", body)
		}
	})

	t.Run("after write skips json", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		if _, err := c.Writer.Write([]byte("tar-bytes")); err != nil {
			t.Fatal(err)
		}
		writeDownloadError(c, errors.New("boom"), "镜像下载失败")
		if got := w.Body.String(); got != "tar-bytes" {
			t.Fatalf("body corrupted: %q", got)
		}
	})
}

// fakeBatchStager 构造一个不访问网络的暂存器：failRef 返回失败，其余写入一条配置条目。
func fakeBatchStager(failRef string) imageStager {
	return func(ctx context.Context, tw *tar.Writer, imageRef string, options *StreamOptions) (map[string]interface{}, map[string]map[string]string, error) {
		if imageRef == failRef {
			return nil, nil, errors.New("upstream unavailable")
		}
		configName := strings.ReplaceAll(imageRef, ":", "_") + ".json"
		if err := writeTarText(tw, configName, `{"architecture":"amd64"}`); err != nil {
			return nil, nil, err
		}
		manifest := map[string]interface{}{
			"Config":   configName,
			"RepoTags": []string{imageRef},
			"Layers":   []string{},
		}
		repositories := map[string]map[string]string{
			imageRef: {"1": "sha256:abc"},
		}
		return manifest, repositories, nil
	}
}

// readTarEntries 读取 tar 全部条目，返回 名称→内容 的映射；tar 非法时测试失败。
func readTarEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	entries := make(map[string]string)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar 解析失败: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读取条目 %s 失败: %v", hdr.Name, err)
		}
		entries[hdr.Name] = string(body)
	}
	return entries
}

func TestStreamMultipleImagesContinuesOnFailure(t *testing.T) {
	is := &ImageStreamer{concurrency: 1}
	refs := []string{"nginx:1", "bad/image:2", "redis:3"}

	var buf bytes.Buffer
	report, err := is.streamMultipleImagesWithStager(
		context.Background(), refs, &buf,
		&StreamOptions{UseCompressedLayers: true},
		fakeBatchStager("bad/image:2"),
	)
	if err != nil {
		t.Fatalf("批量下载不应因单个镜像失败而返回错误: %v", err)
	}

	if report.Total != 3 || report.SuccessCount != 2 || report.FailureCount != 1 {
		t.Fatalf("报告计数错误: %+v", report)
	}
	if len(report.Succeeded) != 2 || report.Succeeded[0] != "nginx:1" || report.Succeeded[1] != "redis:3" {
		t.Fatalf("成功列表错误: %+v", report.Succeeded)
	}
	if len(report.Failed) != 1 || report.Failed[0].Image != "bad/image:2" ||
		!strings.Contains(report.Failed[0].Error, "upstream unavailable") {
		t.Fatalf("失败列表错误: %+v", report.Failed)
	}

	entries := readTarEntries(t, buf.Bytes())

	for _, name := range []string{"nginx_1.json", "redis_3.json"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("缺少成功镜像条目 %s", name)
		}
	}
	if _, ok := entries["bad_image_2.json"]; ok {
		t.Error("失败镜像的条目不应出现在 tar 中")
	}
	for _, name := range []string{"manifest.json", "repositories", "download-report.json", "download-report.txt"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("缺少 tar 条目 %s", name)
		}
	}

	var manifests []map[string]interface{}
	if err := json.Unmarshal([]byte(entries["manifest.json"]), &manifests); err != nil {
		t.Fatalf("manifest.json 非法: %v", err)
	}
	if len(manifests) != 2 {
		t.Fatalf("manifest 条目数 = %d, 期望 2", len(manifests))
	}

	var tarReport BatchReport
	if err := json.Unmarshal([]byte(entries["download-report.json"]), &tarReport); err != nil {
		t.Fatalf("download-report.json 非法: %v", err)
	}
	if tarReport.SuccessCount != 2 || tarReport.FailureCount != 1 {
		t.Fatalf("包内报告计数错误: %+v", tarReport)
	}

	textReport := entries["download-report.txt"]
	if !strings.Contains(textReport, "[OK]   nginx:1") ||
		!strings.Contains(textReport, "[FAIL] bad/image:2") ||
		!strings.Contains(textReport, "upstream unavailable") {
		t.Fatalf("文本报告缺少明细:\n%s", textReport)
	}
}

func TestStreamMultipleImagesAllFail(t *testing.T) {
	is := &ImageStreamer{concurrency: 1}
	refs := []string{"bad/image:1", "bad/image:2"}

	alwaysFail := func(ctx context.Context, tw *tar.Writer, imageRef string, options *StreamOptions) (map[string]interface{}, map[string]map[string]string, error) {
		return nil, nil, errors.New("not found")
	}

	for _, compressed := range []bool{false, true} {
		var buf bytes.Buffer
		report, err := is.streamMultipleImagesWithStager(
			context.Background(), refs, &buf,
			&StreamOptions{UseCompressedLayers: true, Compression: compressed}, alwaysFail,
		)
		if err != nil {
			t.Fatalf("全部失败也不应中断流程(compressed=%v): %v", compressed, err)
		}
		if buf.Len() != 0 {
			t.Fatalf("全部失败时不应写出任何数据(compressed=%v)，实际写出 %d 字节", compressed, buf.Len())
		}
		if report.SuccessCount != 0 || report.FailureCount != 2 || report.Total != 2 {
			t.Fatalf("报告计数错误(compressed=%v): %+v", compressed, report)
		}
		if report.Failed[0].Image != "bad/image:1" || report.Failed[1].Image != "bad/image:2" {
			t.Fatalf("失败列表错误(compressed=%v): %+v", compressed, report.Failed)
		}
	}
}

func TestStreamMultipleImagesCompressedOutput(t *testing.T) {
	is := &ImageStreamer{concurrency: 1}
	var buf bytes.Buffer
	report, err := is.streamMultipleImagesWithStager(
		context.Background(), []string{"nginx:1"}, &buf,
		&StreamOptions{UseCompressedLayers: true, Compression: true},
		fakeBatchStager(""),
	)
	if err != nil {
		t.Fatalf("压缩批量打包失败: %v", err)
	}
	if report.SuccessCount != 1 {
		t.Fatalf("成功计数错误: %+v", report)
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatalf("输出不是合法 gzip: %v", err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("读取 gzip 内容失败: %v", err)
	}
	entries := readTarEntries(t, decompressed)
	if _, ok := entries["download-report.json"]; !ok {
		t.Fatal("gzip 内的 tar 缺少下载报告")
	}
}

func newBatchTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	InitDebouncer()
	r := gin.New()
	InitImageTarRoutes(r)
	return r
}

func TestBatchPrepareRejectsOverLimit(t *testing.T) {
	r := newBatchTestRouter()

	var images []string
	for i := 0; i < 11; i++ {
		images = append(images, fmt.Sprintf("nginx:tag-%d", i))
	}

	body, _ := json.Marshal(map[string]interface{}{"images": images})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/batch?mode=prepare", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("超量批量请求状态码 = %d, 期望 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "超过限制") {
		t.Fatalf("应提示数量超限，body=%s", w.Body.String())
	}
}

func TestBatchPrepareAcceptsWithinLimit(t *testing.T) {
	r := newBatchTestRouter()

	body, _ := json.Marshal(map[string]interface{}{
		"images":              []string{"nginx:prepare-a", "nginx:prepare-b"},
		"useCompressedLayers": true,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/image/batch?mode=prepare", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if !strings.Contains(resp["download_url"], "/api/image/batch?token=") {
		t.Fatalf("download_url 异常: %q", resp["download_url"])
	}
}

func TestLazyHeaderWriterDefersHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/image/batch?token=x", nil)

	lw := &lazyHeaderWriter{c: c, filename: "batch_1_images.tar", compressed: false}
	if got := w.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("未写入前不应设置下载头，实际: %q", got)
	}
	if _, err := lw.Write([]byte("tar-bytes")); err != nil {
		t.Fatal(err)
	}
	if !lw.written {
		t.Fatal("写入后 written 应为 true")
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "batch_1_images.tar") {
		t.Fatalf("写入后应设置下载头，实际: %q", got)
	}
	if got := w.Body.String(); got != "tar-bytes" {
		t.Fatalf("响应体被篡改: %q", got)
	}
}

func TestRenderBatchReportText(t *testing.T) {
	report := &BatchReport{
		Platform:     "linux/amd64",
		Total:        2,
		SuccessCount: 1,
		FailureCount: 1,
		Succeeded:    []string{"nginx:1"},
		Failed:       []BatchImageFailure{{Image: "bad:2", Error: "boom"}},
		GeneratedAt:  time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC),
	}
	text := renderBatchReportText(report)
	for _, want := range []string{"总数: 2，成功: 1，失败: 1", "[OK]   nginx:1", "[FAIL] bad:2", "原因: boom"} {
		if !strings.Contains(text, want) {
			t.Fatalf("报告缺少 %q:\n%s", want, text)
		}
	}
}
