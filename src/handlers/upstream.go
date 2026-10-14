package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"hubproxy/config"
	"hubproxy/utils"
)

// errAllUpstreamsUnavailable 所有候选上游均尝试失败（连接、超时或 5xx）。
var errAllUpstreamsUnavailable = errors.New("所有上游均不可用")

// terminalUpstreamError 包装明确不应重试的错误（如非法引用、镜像不存在等）。
type terminalUpstreamError struct {
	error
}

func (e *terminalUpstreamError) Unwrap() error { return e.error }

// refLocator 描述一个逻辑镜像在上游池中的定位信息
type refLocator struct {
	pool  string   // 健康管理器中的池名称
	hosts []string // 配置的上游主机（按优先级）
	repo  string   // 不含主机、tag、digest 的仓库路径
}

// locatorForRef 从镜像引用推断故障切换池与仓库路径。
// 第二个返回值为 false 表示引用指向未配置的第三方 Registry，只能直连。
func locatorForRef(ref name.Reference) (refLocator, bool) {
	cfg := config.GetConfig()
	reg := ref.Context().RegistryStr()
	repo := ref.Context().RepositoryStr()

	switch reg {
	case name.DefaultRegistry, "registry-1.docker.io":
		return refLocator{pool: utils.DockerHubPool, hosts: cfg.Upstream.DockerHubUpstreams, repo: repo}, true
	}

	if mapping, ok := cfg.Registries[reg]; ok && mapping.Enabled {
		return refLocator{pool: reg, hosts: mapping.UpstreamList(), repo: repo}, true
	}
	return refLocator{}, false
}

// locatorForRegistry 为 /v2 代理路径构造定位信息。
func locatorForRegistry(pool string, hosts []string, repo string) refLocator {
	return refLocator{pool: pool, hosts: hosts, repo: repo}
}

// candidateHosts 返回当前请求使用的候选上游快照。
// 快照在请求开始时确定，之后摘除/恢复上游都不会影响本次请求。
func (l refLocator) candidateHosts() []string {
	return utils.GlobalUpstreamManager.CandidateHosts(l.pool, l.hosts)
}

// rebasedRef 用指定上游主机重建镜像引用。
func (l refLocator) rebasedRef(host, reference string) (name.Reference, error) {
	return parseRebasedRef(host, l.repo, reference)
}

// parseRebasedRef 根据 reference 是否为 digest 构造 tag/digest 引用。
func parseRebasedRef(host, repo, reference string) (name.Reference, error) {
	if strings.HasPrefix(reference, "sha256:") {
		return name.NewDigest(fmt.Sprintf("%s/%s@%s", host, repo, reference))
	}
	return name.NewTag(fmt.Sprintf("%s/%s:%s", host, repo, reference))
}

// rebasedDigestRef 用指定上游主机重建 digest 引用。
func (l refLocator) rebasedDigestRef(host, digest string) (name.Digest, error) {
	return name.NewDigest(fmt.Sprintf("%s/%s@%s", host, l.repo, digest))
}

// baseRemoteOptions 所有上游共享的 go-containerregistry 选项。
// 重试退避收敛为一次 500ms 等待，随后由本服务切换到下一个上游，
// 避免在已挂掉的单个上游上阻塞 1s+3s。
func baseRemoteOptions() []remote.Option {
	return []remote.Option{
		remote.WithAuth(authn.Anonymous),
		remote.WithUserAgent("hubproxy/go-containerregistry"),
		remote.WithTransport(utils.GetGlobalHTTPClient().Transport),
		remote.WithRetryBackoff(remote.Backoff{
			Steps:    2,
			Duration: 500 * time.Millisecond,
			Factor:   2.0,
		}),
	}
}

// upstreamErrorKind 上游调用错误的分类，决定故障切换行为
type upstreamErrorKind int

const (
	kindOK          upstreamErrorKind = iota // 调用成功
	kindNotFound                             // 上游明确响应内容不存在（404 等），可尝试下一个镜像源
	kindTerminal                             // 不应切换（非法引用、401/403、客户端取消等）
	kindUnavailable                          // 上游不可用（连接失败、超时、429/5xx）
)

// notFoundErrorCodes 表示内容在当前镜像源不存在、但可能在其他镜像源存在的错误码
var notFoundErrorCodes = map[transport.ErrorCode]struct{}{
	transport.ManifestUnknownErrorCode:     {},
	transport.NameUnknownErrorCode:         {},
	transport.BlobUnknownErrorCode:         {},
	transport.ManifestBlobUnknownErrorCode: {},
}

// terminalErrorCodes 表示切换镜像源也无法解决的错误码
var terminalErrorCodes = map[transport.ErrorCode]struct{}{
	transport.UnauthorizedErrorCode:      {},
	transport.DeniedErrorCode:            {},
	transport.UnsupportedErrorCode:       {},
	transport.TagInvalidErrorCode:        {},
	transport.DigestInvalidErrorCode:     {},
	transport.BlobUploadInvalidErrorCode: {},
}

// classifyUpstreamError 判断上游调用错误的故障切换分类。
func classifyUpstreamError(err error) upstreamErrorKind {
	if err == nil {
		return kindOK
	}
	var terminal *terminalUpstreamError
	if errors.As(err, &terminal) {
		return kindTerminal
	}
	if errors.Is(err, context.Canceled) {
		return kindTerminal
	}

	var transportErr *transport.Error
	if errors.As(err, &transportErr) {
		for _, diagnostic := range transportErr.Errors {
			if _, ok := notFoundErrorCodes[diagnostic.Code]; ok {
				return kindNotFound
			}
			if _, ok := terminalErrorCodes[diagnostic.Code]; ok {
				return kindTerminal
			}
		}
		switch transportErr.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			return kindNotFound
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusMethodNotAllowed:
			return kindTerminal
		default:
			return kindUnavailable
		}
	}

	// 非结构化错误兜底：识别明确的 registry 错误码，其余一律按上游不可用处理。
	message := strings.ToUpper(err.Error())
	for code := range notFoundErrorCodes {
		if strings.Contains(message, string(code)) {
			return kindNotFound
		}
	}
	for code := range terminalErrorCodes {
		if strings.Contains(message, string(code)) {
			return kindTerminal
		}
	}
	return kindUnavailable
}

// isTerminalUpstreamError 判断错误是否不应触发任何切换。
func isTerminalUpstreamError(err error) bool {
	return classifyUpstreamError(err) == kindTerminal
}

// withUpstreamFailover 按候选顺序逐个尝试上游。
//   - 成功：标记健康并返回；
//   - 内容不存在（404）：上游存活，继续尝试下一个镜像源（pull-through 缓存可能未同步）；
//   - 终端错误：直接返回；
//   - 上游不可用：标记失败并切换下一个；
//   - 全部不存在：返回最后的不存在错误；全部不可用：返回 errAllUpstreamsUnavailable。
func withUpstreamFailover(loc refLocator, call func(host string) error) error {
	candidates := loc.candidateHosts()
	var notFoundErr error
	var unavailableErr error

	for _, host := range candidates {
		err := call(host)
		if err == nil {
			utils.GlobalUpstreamManager.RecordSuccess(loc.pool, host)
			return nil
		}

		switch classifyUpstreamError(err) {
		case kindTerminal:
			return err
		case kindNotFound:
			// 上游给出了明确响应，说明它存活，不计入熔断失败
			utils.GlobalUpstreamManager.RecordSuccess(loc.pool, host)
			notFoundErr = err
		default:
			fmt.Printf("上游 %s 请求失败，准备切换下一个上游: %v\n", host, err)
			utils.GlobalUpstreamManager.RecordFailure(loc.pool, host)
			unavailableErr = err
		}
	}

	// 既遇到过不可用又遇到过不存在：优先报告不可用，触发客户端重试
	if unavailableErr != nil {
		return fmt.Errorf("%w: %v", errAllUpstreamsUnavailable, unavailableErr)
	}
	if notFoundErr != nil {
		return notFoundErr
	}
	return fmt.Errorf("%w: 没有配置可用的上游", errAllUpstreamsUnavailable)
}

// blobOpener 在指定上游打开一个按 digest 寻址的 blob 下载，返回声明大小与数据流。
type blobOpener func(host string) (size int64, body io.ReadCloser, err error)

// failoverBlobReader 在首字节传输前透明切换上游；一旦已向下游写出数据，
// 立即锁定当前上游（blob 按 digest 寻址，但中断的字节流无法透明续传）。
// 这样切换健康状态永远不会破坏正在进行的下载。
type failoverBlobReader struct {
	pool       string
	candidates []string
	open       blobOpener

	current     io.ReadCloser
	currentHost string
	size        int64
	index       int
	started     bool
}

// newFailoverBlobReader 在首个可用上游上打开 blob。
func newFailoverBlobReader(loc refLocator, open blobOpener) (*failoverBlobReader, error) {
	reader := &failoverBlobReader{
		pool:       loc.pool,
		candidates: loc.candidateHosts(),
		open:       open,
	}
	if err := reader.openNext(); err != nil {
		return nil, err
	}
	return reader, nil
}

// Size 返回成功打开的上游声明的 blob 大小（同一 digest 在各镜像源间一致）。
func (r *failoverBlobReader) Size() int64 { return r.size }

func (r *failoverBlobReader) openNext() error {
	var notFoundErr error
	var unavailableErr error

	for r.index < len(r.candidates) {
		host := r.candidates[r.index]
		size, body, err := r.open(host)
		if err == nil {
			r.current = body
			r.currentHost = host
			r.size = size
			utils.GlobalUpstreamManager.RecordSuccess(r.pool, host)
			return nil
		}

		switch classifyUpstreamError(err) {
		case kindTerminal:
			return err
		case kindNotFound:
			// 该镜像源没有这个 blob（如未同步的 pull-through 缓存），尝试下一个
			utils.GlobalUpstreamManager.RecordSuccess(r.pool, host)
			notFoundErr = err
		default:
			fmt.Printf("上游 %s 打开 blob 失败，准备切换下一个上游: %v\n", host, err)
			utils.GlobalUpstreamManager.RecordFailure(r.pool, host)
			unavailableErr = err
		}
		r.index++
	}

	if unavailableErr != nil {
		return fmt.Errorf("%w: 无法在任何上游打开 blob: %v", errAllUpstreamsUnavailable, unavailableErr)
	}
	if notFoundErr != nil {
		return notFoundErr
	}
	return fmt.Errorf("%w: 没有配置可用的上游", errAllUpstreamsUnavailable)
}

// Read 实现 io.Reader：首字节前失败自动切换，首字节后锁定当前上游。
func (r *failoverBlobReader) Read(p []byte) (int, error) {
	for {
		n, err := r.current.Read(p)
		if n > 0 {
			r.started = true
			return n, nil
		}
		if err == nil || r.started {
			return n, err
		}
		// 响应头已成功返回，立即 EOF 代表合法的空 blob，不能切换
		if errors.Is(err, io.EOF) {
			return n, err
		}
		if errors.Is(err, context.Canceled) {
			return n, err
		}

		// 尚未向下游写出任何字节：关闭当前连接，切换下一个上游从头开始
		fmt.Printf("blob 下载在首字节前中断，切换下一个上游: %v\n", err)
		utils.GlobalUpstreamManager.RecordFailure(r.pool, r.currentHost)
		_ = r.current.Close()
		r.index++
		if nextErr := r.openNext(); nextErr != nil {
			return 0, nextErr
		}
	}
}

// Close 关闭当前上游连接。
func (r *failoverBlobReader) Close() error {
	if r.current != nil {
		return r.current.Close()
	}
	return nil
}

// openBlobWithFailover 打开按 digest 寻址的 blob（layer 或 config），自动故障切换。
// compressed=false 时返回解压后的数据流与未压缩大小。
func openBlobWithFailover(loc refLocator, digest string, compressed bool, options []remote.Option) (*failoverBlobReader, error) {
	open := func(host string) (int64, io.ReadCloser, error) {
		digestRef, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", host, loc.repo, digest))
		if err != nil {
			return 0, nil, &terminalUpstreamError{err}
		}

		layer, err := remote.Layer(digestRef, options...)
		if err != nil {
			return 0, nil, err
		}

		if compressed {
			size, err := layer.Size()
			if err != nil {
				return 0, nil, err
			}
			body, err := layer.Compressed()
			if err != nil {
				return 0, nil, err
			}
			return size, body, nil
		}

		size, err := partial.UncompressedSize(layer)
		if err != nil {
			return 0, nil, err
		}
		body, err := layer.Uncompressed()
		if err != nil {
			return 0, nil, err
		}
		return size, body, nil
	}

	return newFailoverBlobReader(loc, open)
}

// remoteGetWithFailover 在多个上游间尝试获取镜像描述符（manifest 请求）。
func remoteGetWithFailover(loc refLocator, reference string, options []remote.Option) (*remote.Descriptor, error) {
	var descriptor *remote.Descriptor
	err := withUpstreamFailover(loc, func(host string) error {
		ref, err := loc.rebasedRef(host, reference)
		if err != nil {
			return &terminalUpstreamError{err}
		}
		descriptor, err = remote.Get(ref, options...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return descriptor, nil
}

// remoteHeadWithFailover 在多个上游间尝试 HEAD manifest。
func remoteHeadWithFailover(loc refLocator, reference string, options []remote.Option) (*v1.Descriptor, error) {
	var descriptor *v1.Descriptor
	err := withUpstreamFailover(loc, func(host string) error {
		ref, err := loc.rebasedRef(host, reference)
		if err != nil {
			return &terminalUpstreamError{err}
		}
		descriptor, err = remote.Head(ref, options...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return descriptor, nil
}

// remoteListWithFailover 在多个上游间尝试列出 tags。
func remoteListWithFailover(loc refLocator, options []remote.Option) ([]string, error) {
	var tags []string
	err := withUpstreamFailover(loc, func(host string) error {
		repo, err := name.NewRepository(fmt.Sprintf("%s/%s", host, loc.repo))
		if err != nil {
			return &terminalUpstreamError{err}
		}
		tags, err = remote.List(repo, options...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}
