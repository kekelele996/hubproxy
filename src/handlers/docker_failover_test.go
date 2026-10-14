package handlers

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/utils"
)

// newFakeRegistry 启动一个最小的 Registry v2 假服务（全部走本地回环，无外部依赖）。
func newFakeRegistry(t *testing.T, blobData []byte, blobDigest string) *httptest.Server {
	t.Helper()
	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      2,
			"digest":    "sha256:" + strings.Repeat("c", 64),
		},
		"layers": []map[string]interface{}{{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"size":      len(blobData),
			"digest":    blobDigest,
		}},
	}
	manifestBody, _ := json.Marshal(manifest)
	return newRegistryServer(t, manifestBody, "application/vnd.docker.distribution.manifest.v2+json",
		map[string][]byte{blobDigest: blobData}, true)
}

// newRegistryServer 构造按 digest 提供多个 blob 的假 Registry。
func newRegistryServer(t *testing.T, manifestBody []byte, manifestType string, blobs map[string][]byte, withTags bool) *httptest.Server {
	t.Helper()
	manifestDigest := "sha256:" + sha256Hex(manifestBody)

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", manifestType)
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(manifestBody)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifestBody)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			digest := r.URL.Path[strings.Index(r.URL.Path, "/blobs/")+len("/blobs/"):]
			data, ok := blobs[digest]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", digest)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		case withTags && strings.HasSuffix(r.URL.Path, "/tags/list"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"library/nginx","tags":["latest"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	return httptest.NewServer(mux)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func deadLocalhost(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func newFailoverTestRouter(t *testing.T, dead, alive string) *gin.Engine {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	data := fmt.Sprintf(`
[server]
enableFrontend = false

[upstream]
dockerHubUpstreams = ["%s", "%s"]
failureThreshold = 1
cooldown = "1m"
healthCheckEnabled = false

[tokenCache]
enabled = false
`, dead, alive)
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	utils.InitHTTPClients()
	utils.InitUpstreamManager()
	t.Cleanup(func() { utils.GlobalUpstreamManager.Stop() })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Any("/v2/*path", ProxyDockerRegistryGin)
	return router
}

func serveFailoverRequest(router *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestV2FailoverManifestSwitchesToAliveUpstream 主上游不可用时，manifest 请求自动切换到备用上游。
func TestV2FailoverManifestSwitchesToAliveUpstream(t *testing.T) {
	var gzipBuf bytes.Buffer
	gw := gzip.NewWriter(&gzipBuf)
	_, _ = gw.Write([]byte("fake-layer-content"))
	_ = gw.Close()
	blobData := gzipBuf.Bytes()
	blobDigest := "sha256:" + sha256Hex(blobData)

	alive := newFakeRegistry(t, blobData, blobDigest)
	defer alive.Close()
	dead := deadLocalhost(t)

	router := newFailoverTestRouter(t, dead, alive.URL[len("http://"):])

	w := serveFailoverRequest(router, "/v2/library/nginx/manifests/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Docker-Content-Digest") == "" {
		t.Fatalf("缺少 Docker-Content-Digest 响应头")
	}

	// 主上游被熔断摘除，备用上游健康
	status := map[string]bool{}
	for _, s := range utils.GlobalUpstreamManager.PoolStatus(utils.DockerHubPool) {
		status[s.Host] = s.Healthy
	}
	if status[dead] {
		t.Fatal("故障主上游应被标记为不健康")
	}
	if !status[alive.URL[len("http://"):]] {
		t.Fatal("备用上游应保持健康")
	}
}

// TestV2FailoverBlobSwitchesBeforeFirstByte blob 首字节前失败自动切换，下载内容完整。
func TestV2FailoverBlobSwitchesBeforeFirstByte(t *testing.T) {
	var gzipBuf bytes.Buffer
	gw := gzip.NewWriter(&gzipBuf)
	_, _ = gw.Write([]byte("fake-layer-content-for-blob"))
	_ = gw.Close()
	blobData := gzipBuf.Bytes()
	blobDigest := "sha256:" + sha256Hex(blobData)

	alive := newFakeRegistry(t, blobData, blobDigest)
	defer alive.Close()
	dead := deadLocalhost(t)

	router := newFailoverTestRouter(t, dead, alive.URL[len("http://"):])

	w := serveFailoverRequest(router, "/v2/library/nginx/blobs/"+blobDigest)
	if w.Code != http.StatusOK {
		t.Fatalf("blob status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Equal(w.Body.Bytes(), blobData) {
		t.Fatalf("blob 内容不一致: got %d bytes, want %d bytes", w.Body.Len(), len(blobData))
	}
	if w.Header().Get("Docker-Content-Digest") != blobDigest {
		t.Fatalf("digest 响应头 = %q, want %q", w.Header().Get("Docker-Content-Digest"), blobDigest)
	}
}

// TestV2FailoverTagsSwitches tags 列表请求同样自动切换。
func TestV2FailoverTagsSwitches(t *testing.T) {
	alive := newFakeRegistry(t, nil, "sha256:"+strings.Repeat("d", 64))
	defer alive.Close()
	dead := deadLocalhost(t)

	router := newFailoverTestRouter(t, dead, alive.URL[len("http://"):])

	w := serveFailoverRequest(router, "/v2/library/nginx/tags/list")
	if w.Code != http.StatusOK {
		t.Fatalf("tags status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "latest") || !strings.Contains(body, "library/nginx") {
		t.Fatalf("tags 响应异常: %s", body)
	}
}

// TestV2AllUpstreamsDownReturnsBadGateway 所有上游均不可用时返回 502，而不是 404。
func TestV2AllUpstreamsDownReturnsBadGateway(t *testing.T) {
	dead1 := deadLocalhost(t)
	dead2 := deadLocalhost(t)

	router := newFailoverTestRouter(t, dead1, dead2)

	w := serveFailoverRequest(router, "/v2/library/nginx/manifests/latest")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("全部上游不可用时 status = %d, want 502; body=%s", w.Code, w.Body.String())
	}

	// blob 路径同样返回 502
	digest := "sha256:" + strings.Repeat("e", 64)
	w = serveFailoverRequest(router, "/v2/library/nginx/blobs/"+digest)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("blob 全部上游不可用时 status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
}

// TestV2FailoverNotFoundTriesNextMirror 第一个镜像源未同步（404）时，继续从备用镜像源拉取。
func TestV2FailoverNotFoundTriesNextMirror(t *testing.T) {
	var gzipBuf bytes.Buffer
	gw := gzip.NewWriter(&gzipBuf)
	_, _ = gw.Write([]byte("fake-layer-content"))
	_ = gw.Close()
	blobData := gzipBuf.Bytes()
	blobDigest := "sha256:" + sha256Hex(blobData)

	alive := newFakeRegistry(t, blobData, blobDigest)
	defer alive.Close()

	// 未同步的 pull-through 镜像缓存：manifest 返回 404
	staleMux := http.NewServeMux()
	staleMux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	stale := httptest.NewServer(staleMux)
	defer stale.Close()

	router := newFailoverTestRouter(t, stale.URL[len("http://"):], alive.URL[len("http://"):])

	w := serveFailoverRequest(router, "/v2/library/nginx/manifests/latest")
	if w.Code != http.StatusOK {
		t.Fatalf("备用镜像源存在 manifest 时应成功, got %d: %s", w.Code, w.Body.String())
	}

	// 404 是有效响应，不应触发熔断：两个镜像源都应保持健康
	for _, s := range utils.GlobalUpstreamManager.PoolStatus(utils.DockerHubPool) {
		if !s.Healthy {
			t.Fatalf("404 不应熔断镜像源 %s", s.Host)
		}
	}
}

// TestV2AllMirrorsNotFoundReturns404 所有镜像源都没有该镜像时才返回 404。
func TestV2AllMirrorsNotFoundReturns404(t *testing.T) {
	notFoundMux := http.NewServeMux()
	notFoundMux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	first := httptest.NewServer(notFoundMux)
	defer first.Close()
	second := httptest.NewServer(notFoundMux)
	defer second.Close()

	router := newFailoverTestRouter(t, first.URL[len("http://"):], second.URL[len("http://"):])

	w := serveFailoverRequest(router, "/v2/library/nginx/manifests/latest")
	if w.Code != http.StatusNotFound {
		t.Fatalf("全部镜像源 404 时应返回 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestV2FailoverStopsOnUnauthorized 401 属于终端错误，不应逐个镜像源重试。
func TestV2FailoverStopsOnUnauthorized(t *testing.T) {
	authMux := http.NewServeMux()
	authMux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`)
	})
	authServer := httptest.NewServer(authMux)
	defer authServer.Close()

	// 备用镜像源对任意请求返回 200：一旦错误地发生故障切换，整体就会成功
	aliveHits := 0
	secondMux := http.NewServeMux()
	secondMux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		aliveHits++
		w.WriteHeader(http.StatusOK)
	})
	second := httptest.NewServer(secondMux)
	defer second.Close()

	router := newFailoverTestRouter(t, authServer.URL[len("http://"):], second.URL[len("http://"):])

	w := serveFailoverRequest(router, "/v2/library/nginx/manifests/latest")
	if w.Code != http.StatusNotFound {
		t.Fatalf("401 应终止切换并返回 404, got %d: %s", w.Code, w.Body.String())
	}
	if aliveHits != 0 {
		t.Fatalf("401 不应继续尝试下一个镜像源, 备用命中 %d 次", aliveHits)
	}
}
