package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"hubproxy/config"
	"hubproxy/utils"
)

type errReadCloser struct {
	data []byte
	err  error
}

func (e *errReadCloser) Read(p []byte) (int, error) {
	if len(e.data) > 0 {
		n := copy(p, e.data)
		e.data = e.data[n:]
		return n, nil
	}
	return 0, e.err
}

func (e *errReadCloser) Close() error { return nil }

func useTestUpstreamManager(t *testing.T, hosts ...string) *utils.UpstreamManager {
	t.Helper()
	previous := utils.GlobalUpstreamManager
	m := utils.NewUpstreamManager(utils.UpstreamManagerConfig{
		Pools:              []utils.PoolConfig{{Name: "p", Hosts: hosts}},
		FailureThreshold:   1,
		Cooldown:           time.Minute,
		HealthCheckEnabled: false,
	}, nil)
	utils.GlobalUpstreamManager = m
	t.Cleanup(func() {
		m.Stop()
		utils.GlobalUpstreamManager = previous
	})
	return m
}

func TestClassifyUpstreamError(t *testing.T) {
	if classifyUpstreamError(nil) != kindOK {
		t.Fatal("nil 应为 kindOK")
	}

	notFound := &transport.Error{StatusCode: 404, Errors: []transport.Diagnostic{{Code: transport.ManifestUnknownErrorCode}}}
	if classifyUpstreamError(notFound) != kindNotFound {
		t.Fatal("MANIFEST_UNKNOWN/404 应为内容不存在（可尝试其他镜像源）")
	}
	if isTerminalUpstreamError(notFound) {
		t.Fatal("404 不是终端错误")
	}

	blobMissing := &transport.Error{StatusCode: 404, Errors: []transport.Diagnostic{{Code: transport.BlobUnknownErrorCode}}}
	if classifyUpstreamError(blobMissing) != kindNotFound {
		t.Fatal("BLOB_UNKNOWN 应为内容不存在")
	}

	unauthorized := &transport.Error{StatusCode: 401, Errors: []transport.Diagnostic{{Code: transport.UnauthorizedErrorCode}}}
	if classifyUpstreamError(unauthorized) != kindTerminal {
		t.Fatal("401 应为终端错误")
	}

	if classifyUpstreamError(&transport.Error{StatusCode: 502}) != kindUnavailable {
		t.Fatal("502 应为上游不可用")
	}
	if classifyUpstreamError(&transport.Error{StatusCode: 429}) != kindUnavailable {
		t.Fatal("429 应为上游不可用")
	}

	if classifyUpstreamError(errors.New("GET https://x: MANIFEST_UNKNOWN: manifest unknown")) != kindNotFound {
		t.Fatal("字符串形式的 MANIFEST_UNKNOWN 应识别为内容不存在")
	}
	if classifyUpstreamError(errors.New("dial tcp 10.0.0.1:443: connect: connection refused")) != kindUnavailable {
		t.Fatal("连接拒绝应为上游不可用")
	}
	if classifyUpstreamError(context.Canceled) != kindTerminal {
		t.Fatal("客户端取消应为终端错误")
	}
}

func TestWithUpstreamFailoverSwitchesAndReportsHealth(t *testing.T) {
	m := useTestUpstreamManager(t, "a", "b")

	called := []string{}
	err := withUpstreamFailover(refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}, func(host string) error {
		called = append(called, host)
		if host == "a" {
			return errors.New("dial tcp: connect: connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("切换到备用上游后应成功: %v", err)
	}
	if len(called) != 2 || called[0] != "a" || called[1] != "b" {
		t.Fatalf("调用顺序 = %v", called)
	}

	status := map[string]utils.HostStatus{}
	for _, s := range m.PoolStatus("p") {
		status[s.Host] = s
	}
	if status["a"].Healthy {
		t.Fatal("a 应被标记为不健康")
	}
	if !status["b"].Healthy {
		t.Fatal("b 应保持健康")
	}
}

func TestWithUpstreamFailoverStopsOnTerminalError(t *testing.T) {
	useTestUpstreamManager(t, "a", "b")

	called := []string{}
	err := withUpstreamFailover(refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}, func(host string) error {
		called = append(called, host)
		return &transport.Error{StatusCode: 401, Errors: []transport.Diagnostic{{Code: transport.UnauthorizedErrorCode}}}
	})
	if err == nil {
		t.Fatal("终端错误应返回")
	}
	if len(called) != 1 || called[0] != "a" {
		t.Fatalf("401 终端错误不应切换下一个上游, called=%v", called)
	}
}

func TestWithUpstreamFailoverRetriesNotFoundOnNextMirror(t *testing.T) {
	m := useTestUpstreamManager(t, "a", "b")

	called := []string{}
	err := withUpstreamFailover(refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}, func(host string) error {
		called = append(called, host)
		if host == "a" {
			return &transport.Error{StatusCode: 404, Errors: []transport.Diagnostic{{Code: transport.ManifestUnknownErrorCode}}}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("备用镜像源存在内容时应成功: %v", err)
	}
	if len(called) != 2 || called[0] != "a" || called[1] != "b" {
		t.Fatalf("404 应继续尝试下一个镜像源, called=%v", called)
	}

	// 404 不计入熔断失败，主镜像源仍保持健康
	status := map[string]utils.HostStatus{}
	for _, s := range m.PoolStatus("p") {
		status[s.Host] = s
	}
	if !status["a"].Healthy {
		t.Fatal("404 是有效响应，不应将镜像源标记为不健康")
	}
}

func TestWithUpstreamFailoverAllNotFoundReturnsLastError(t *testing.T) {
	useTestUpstreamManager(t, "a", "b")

	called := 0
	err := withUpstreamFailover(refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}, func(host string) error {
		called++
		return &transport.Error{StatusCode: 404, Errors: []transport.Diagnostic{{Code: transport.ManifestUnknownErrorCode}}}
	})
	if err == nil || errors.Is(err, errAllUpstreamsUnavailable) {
		t.Fatalf("全部镜像源均返回 404 时应返回原始 404 错误, got %v", err)
	}
	if called != 2 {
		t.Fatalf("应尝试完全部镜像源, called=%d", called)
	}
}

func TestWithUpstreamFailoverAllDown(t *testing.T) {
	useTestUpstreamManager(t, "a", "b")

	err := withUpstreamFailover(refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}, func(host string) error {
		return errors.New("i/o timeout")
	})
	if !errors.Is(err, errAllUpstreamsUnavailable) {
		t.Fatalf("全部失败应返回 errAllUpstreamsUnavailable, got %v", err)
	}
}

func TestFailoverBlobReaderSwitchesBeforeFirstByte(t *testing.T) {
	useTestUpstreamManager(t, "a", "b")

	opened := []string{}
	loc := refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}
	open := func(host string) (int64, io.ReadCloser, error) {
		opened = append(opened, host)
		if host == "a" {
			return 11, &errReadCloser{err: errors.New("unexpected EOF")}, nil
		}
		return 11, io.NopCloser(bytes.NewReader([]byte("hello-world"))), nil
	}

	reader, err := newFailoverBlobReader(loc, open)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("首字节前失败应透明切换: %v", err)
	}
	if string(data) != "hello-world" {
		t.Fatalf("data = %q", data)
	}
	if len(opened) != 2 || opened[0] != "a" || opened[1] != "b" {
		t.Fatalf("opened = %v", opened)
	}
}

func TestFailoverBlobReaderPinsAfterFirstByte(t *testing.T) {
	useTestUpstreamManager(t, "a", "b")

	opened := []string{}
	loc := refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}
	broken := errors.New("connection reset by peer")
	open := func(host string) (int64, io.ReadCloser, error) {
		opened = append(opened, host)
		if host == "a" {
			// 先返回部分数据再中断：下载已开始，不能透明切换
			return 2, &errReadCloser{data: []byte("AB"), err: broken}, nil
		}
		return 11, io.NopCloser(bytes.NewReader([]byte("should-not-be-used"))), nil
	}

	reader, err := newFailoverBlobReader(loc, open)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	buf := make([]byte, 2)
	n, err := reader.Read(buf)
	if n != 2 || err != nil || string(buf) != "AB" {
		t.Fatalf("首次读取异常: n=%d err=%v", n, err)
	}

	// 已写出字节后中断：必须锁定当前上游返回错误，而不是切换破坏字节流
	_, err = io.Copy(io.Discard, reader)
	if !errors.Is(err, broken) {
		t.Fatalf("应返回当前上游的中断错误, got %v", err)
	}
	if len(opened) != 1 || opened[0] != "a" {
		t.Fatalf("下载开始后不应切换上游, opened=%v", opened)
	}
}

func TestFailoverBlobReaderEmptyBlobIsValidEOF(t *testing.T) {
	useTestUpstreamManager(t, "a", "b")

	opened := 0
	loc := refLocator{pool: "p", hosts: []string{"a", "b"}, repo: "library/nginx"}
	open := func(host string) (int64, io.ReadCloser, error) {
		opened++
		return 0, io.NopCloser(bytes.NewReader(nil)), nil
	}

	reader, err := newFailoverBlobReader(loc, open)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil || len(data) != 0 {
		t.Fatalf("空 blob 应正常结束, data=%q err=%v", data, err)
	}
	if opened != 1 {
		t.Fatalf("合法空 blob 不应触发切换, opened=%d", opened)
	}
}

func TestRebasedRef(t *testing.T) {
	loc := refLocator{pool: "p", hosts: []string{"a"}, repo: "library/nginx"}

	tagRef, err := loc.rebasedRef("mirror.example", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if got := tagRef.Name(); got != "mirror.example/library/nginx:latest" {
		t.Fatalf("tag ref = %q", got)
	}
	if got := tagRef.String(); got != "mirror.example/library/nginx:latest" {
		t.Fatalf("tag string = %q", got)
	}

	digest := "sha256:" + strings.Repeat("a", 64)
	digestRef, err := loc.rebasedRef("mirror.example", digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := digestRef.Identifier(); got != digest {
		t.Fatalf("digest identifier = %q", got)
	}
}

func TestLocatorForRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	data := []byte(`
[upstream]
dockerHubUpstreams = ["hub-mirror.example.com", "registry-1.docker.io"]

[registries."ghcr.io"]
upstreams = ["ghcr-mirror.example.com", "ghcr.io"]
enabled = true
`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	if err := config.LoadConfig(); err != nil {
		t.Fatal(err)
	}

	ref, err := name.ParseReference("nginx")
	if err != nil {
		t.Fatal(err)
	}
	loc, ok := locatorForRef(ref)
	if !ok {
		t.Fatal("docker hub 引用应能定位")
	}
	if loc.pool != utils.DockerHubPool || loc.repo != "library/nginx" {
		t.Fatalf("docker hub locator = %+v", loc)
	}
	if len(loc.hosts) != 2 || loc.hosts[0] != "hub-mirror.example.com" {
		t.Fatalf("docker hub hosts = %v", loc.hosts)
	}

	ref, err = name.ParseReference("ghcr.io/owner/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	loc, ok = locatorForRef(ref)
	if !ok || loc.pool != "ghcr.io" || loc.repo != "owner/app" {
		t.Fatalf("ghcr locator = %+v ok=%v", loc, ok)
	}
	if len(loc.hosts) != 2 || loc.hosts[0] != "ghcr-mirror.example.com" {
		t.Fatalf("ghcr hosts = %v", loc.hosts)
	}

	ref, err = name.ParseReference("unknown.example.com/x/y:z")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := locatorForRef(ref); ok {
		t.Fatal("未配置的第三方 Registry 应返回 ok=false")
	}
}
