package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"hubproxy/config"
	"hubproxy/utils"
)

// DebounceEntry 防抖条目
type DebounceEntry struct {
	LastRequest time.Time
	UserID      string
}

// DownloadDebouncer 下载防抖器
type DownloadDebouncer struct {
	mu          sync.RWMutex
	entries     map[string]*DebounceEntry
	window      time.Duration
	lastCleanup time.Time
}

// NewDownloadDebouncer 创建下载防抖器
func NewDownloadDebouncer(window time.Duration) *DownloadDebouncer {
	return &DownloadDebouncer{
		entries:     make(map[string]*DebounceEntry),
		window:      window,
		lastCleanup: time.Now(),
	}
}

// ShouldAllow 检查是否应该允许请求
func (d *DownloadDebouncer) ShouldAllow(userID, contentKey string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	key := userID + ":" + contentKey
	now := time.Now()

	if entry, exists := d.entries[key]; exists {
		if now.Sub(entry.LastRequest) < d.window {
			return false
		}
	}

	d.entries[key] = &DebounceEntry{
		LastRequest: now,
		UserID:      userID,
	}

	if time.Since(d.lastCleanup) > 5*time.Minute {
		d.cleanup(now)
		d.lastCleanup = now
	}

	return true
}

// cleanup 清理过期条目
func (d *DownloadDebouncer) cleanup(now time.Time) {
	for key, entry := range d.entries {
		if now.Sub(entry.LastRequest) > d.window*2 {
			delete(d.entries, key)
		}
	}
}

// generateContentFingerprint 生成内容指纹
func generateContentFingerprint(images []string, platform string) string {
	sortedImages := make([]string, len(images))
	copy(sortedImages, images)
	sort.Strings(sortedImages)

	content := strings.Join(sortedImages, "|") + ":" + platform

	hash := md5.Sum([]byte(content))
	return hex.EncodeToString(hash[:])
}

// getUserID 获取用户标识
func getUserID(c *gin.Context) string {
	if sessionID, err := c.Cookie("session_id"); err == nil && sessionID != "" {
		return "session:" + sessionID
	}

	ip := c.ClientIP()
	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = "unknown"
	}

	combined := ip + ":" + userAgent
	hash := md5.Sum([]byte(combined))
	return "ip:" + hex.EncodeToString(hash[:8])
}

func getClientIdentity(c *gin.Context) (string, string) {
	ip := c.ClientIP()
	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = "unknown"
	}
	return ip, userAgent
}

var (
	singleImageDebouncer *DownloadDebouncer
	batchImageDebouncer  *DownloadDebouncer
)

// InitDebouncer 初始化防抖器
func InitDebouncer() {
	singleImageDebouncer = NewDownloadDebouncer(5 * time.Second)
	batchImageDebouncer = NewDownloadDebouncer(60 * time.Second)
}

type BatchDownloadRequest struct {
	Images              []string
	Platform            string
	UseCompressedLayers bool
}

type SingleDownloadRequest struct {
	Image               string
	Platform            string
	UseCompressedLayers bool
}

type tokenEntry[T any] struct {
	Request   T
	ExpiresAt time.Time
	IP        string
	UserAgent string
}

type tokenStore[T any] struct {
	mu      sync.RWMutex
	entries map[string]tokenEntry[T]
}

const downloadTokenTTL = 2 * time.Minute
const downloadTokenMaxEntries = 2000

func newTokenStore[T any]() *tokenStore[T] {
	return &tokenStore[T]{
		entries: make(map[string]tokenEntry[T]),
	}
}

func (s *tokenStore[T]) create(req T, ip, userAgent string) (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	now := time.Now()
	entry := tokenEntry[T]{
		Request:   req,
		ExpiresAt: now.Add(downloadTokenTTL),
		IP:        ip,
		UserAgent: userAgent,
	}

	s.mu.Lock()
	s.cleanup(now)
	if len(s.entries) >= downloadTokenMaxEntries {
		s.mu.Unlock()
		return "", fmt.Errorf("令牌过多，请稍后再试")
	}
	s.entries[token] = entry
	s.mu.Unlock()

	return token, nil
}

func (s *tokenStore[T]) consume(token, ip, userAgent string) (T, bool) {
	var empty T
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.entries[token]
	if !exists {
		return empty, false
	}
	if now.After(entry.ExpiresAt) {
		delete(s.entries, token)
		return empty, false
	}
	if entry.IP != ip || entry.UserAgent != userAgent {
		delete(s.entries, token)
		return empty, false
	}
	delete(s.entries, token)
	return entry.Request, true
}

func (s *tokenStore[T]) cleanup(now time.Time) {
	for token, entry := range s.entries {
		if now.After(entry.ExpiresAt) {
			delete(s.entries, token)
		}
	}
}

var batchDownloadTokens = newTokenStore[BatchDownloadRequest]()
var singleDownloadTokens = newTokenStore[SingleDownloadRequest]()

// ImageStreamer 镜像流式下载器
type ImageStreamer struct {
	concurrency   int
	remoteOptions []remote.Option
}

// ImageStreamerConfig 下载器配置
type ImageStreamerConfig struct {
	Concurrency int
}

// NewImageStreamer 创建镜像下载器
func NewImageStreamer(cfg *ImageStreamerConfig) *ImageStreamer {
	if cfg == nil {
		cfg = &ImageStreamerConfig{}
	}

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		appCfg := config.GetConfig()
		concurrency = appCfg.Download.MaxImages
		if concurrency <= 0 {
			concurrency = 10
		}
	}

	remoteOptions := []remote.Option{
		remote.WithAuth(authn.Anonymous),
		remote.WithTransport(utils.GetGlobalHTTPClient().Transport),
	}

	return &ImageStreamer{
		concurrency:   concurrency,
		remoteOptions: remoteOptions,
	}
}

// StreamOptions 下载选项
type StreamOptions struct {
	Platform            string
	Compression         bool
	UseCompressedLayers bool
}

// StreamImageToWriter 流式下载镜像到Writer
func (is *ImageStreamer) StreamImageToWriter(ctx context.Context, imageRef string, writer io.Writer, options *StreamOptions) error {
	if options == nil {
		options = &StreamOptions{UseCompressedLayers: true}
	}

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("解析镜像引用失败: %w", err)
	}

	log.Printf("开始下载镜像: %s", ref.String())

	contextOptions := append(is.remoteOptions, remote.WithContext(ctx))

	desc, err := is.getImageDescriptor(ref, contextOptions)
	if err != nil {
		return fmt.Errorf("获取镜像描述失败: %w", err)
	}
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		return is.streamMultiArchImage(ctx, desc, writer, options, imageRef)
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		return is.streamSingleImage(ctx, desc, writer, options, imageRef)
	default:
		return is.streamSingleImage(ctx, desc, writer, options, imageRef)
	}
}

// getImageDescriptor 获取镜像描述符
func (is *ImageStreamer) getImageDescriptor(ref name.Reference, options []remote.Option) (*remote.Descriptor, error) {
	return remote.Get(ref, options...)
}

func setDownloadHeaders(c *gin.Context, filename string, compressed bool) {
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Header("Expires", "0")
	if compressed {
		c.Header("Content-Encoding", "gzip")
	}
}

// lazyHeaderWriter 在首次写入响应体时才设置下载响应头，
// 使得批量打包在首个镜像成功前仍可切换为 JSON 错误响应。
type lazyHeaderWriter struct {
	c          *gin.Context
	filename   string
	compressed bool
	written    bool
}

func (w *lazyHeaderWriter) Write(p []byte) (int, error) {
	if !w.written && len(p) > 0 {
		w.written = true
		setDownloadHeaders(w.c, w.filename, w.compressed)
	}
	return w.c.Writer.Write(p)
}

// writeDownloadError 仅在尚未写出响应体时返回 JSON；流已开始则只记日志，避免损坏 tar。
func writeDownloadError(c *gin.Context, err error, message string) {
	if c.Writer.Written() {
		log.Printf("%s: %v", message, err)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": message + ": " + err.Error()})
}

// StreamImageToGin 流式响应到Gin
func (is *ImageStreamer) StreamImageToGin(ctx context.Context, imageRef string, c *gin.Context, options *StreamOptions) error {
	if options == nil {
		options = &StreamOptions{UseCompressedLayers: true}
	}

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("解析镜像引用失败: %w", err)
	}

	contextOptions := append(is.remoteOptions, remote.WithContext(ctx))
	desc, err := is.getImageDescriptor(ref, contextOptions)
	if err != nil {
		return fmt.Errorf("获取镜像描述失败: %w", err)
	}

	filename := strings.ReplaceAll(imageRef, "/", "_") + ".tar"
	setDownloadHeaders(c, filename, options.Compression)

	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		return is.streamMultiArchImage(ctx, desc, c.Writer, options, imageRef)
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		return is.streamSingleImage(ctx, desc, c.Writer, options, imageRef)
	default:
		return is.streamSingleImage(ctx, desc, c.Writer, options, imageRef)
	}
}

// streamMultiArchImage 处理多架构镜像
func (is *ImageStreamer) streamMultiArchImage(ctx context.Context, desc *remote.Descriptor, writer io.Writer, options *StreamOptions, imageRef string) error {
	img, err := is.selectPlatformImage(desc, options)
	if err != nil {
		return err
	}

	return is.streamImageLayers(ctx, img, writer, options, imageRef)
}

// streamSingleImage 处理单架构镜像
func (is *ImageStreamer) streamSingleImage(ctx context.Context, desc *remote.Descriptor, writer io.Writer, options *StreamOptions, imageRef string) error {
	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("获取镜像失败: %w", err)
	}

	return is.streamImageLayers(ctx, img, writer, options, imageRef)
}

// streamImageLayers 处理镜像层
func (is *ImageStreamer) streamImageLayers(ctx context.Context, img v1.Image, writer io.Writer, options *StreamOptions, imageRef string) error {
	var finalWriter io.Writer = writer

	if options.Compression {
		gzWriter := gzip.NewWriter(writer)
		defer gzWriter.Close()
		finalWriter = gzWriter
	}

	tarWriter := tar.NewWriter(finalWriter)
	defer tarWriter.Close()

	configFile, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("获取镜像配置失败: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("获取镜像层失败: %w", err)
	}

	log.Printf("镜像包含 %d 层", len(layers))

	return is.streamDockerFormat(ctx, tarWriter, img, layers, configFile, imageRef, options)
}

// streamDockerFormat 生成Docker格式
func (is *ImageStreamer) streamDockerFormat(ctx context.Context, tarWriter *tar.Writer, img v1.Image, layers []v1.Layer, configFile *v1.ConfigFile, imageRef string, options *StreamOptions) error {
	return is.streamDockerFormatWithReturn(ctx, tarWriter, img, layers, configFile, imageRef, nil, nil, options)
}

// streamDockerFormatWithReturn 生成Docker格式并返回manifest和repositories信息
func (is *ImageStreamer) streamDockerFormatWithReturn(ctx context.Context, tarWriter *tar.Writer, img v1.Image, layers []v1.Layer, configFile *v1.ConfigFile, imageRef string, manifestOut *map[string]interface{}, repositoriesOut *map[string]map[string]string, options *StreamOptions) error {
	configDigest, err := img.ConfigName()
	if err != nil {
		return err
	}

	configData, err := json.Marshal(configFile)
	if err != nil {
		return err
	}

	configHeader := &tar.Header{
		Name: configDigest.String() + ".json",
		Size: int64(len(configData)),
		Mode: 0644,
	}

	if err := tarWriter.WriteHeader(configHeader); err != nil {
		return err
	}
	if _, err := tarWriter.Write(configData); err != nil {
		return err
	}

	layerDigests := make([]string, len(layers))
	for i, layer := range layers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if err := func() error {
			digest, err := layer.Digest()
			if err != nil {
				return err
			}
			layerDigests[i] = digest.String()

			layerDir := digest.String()
			layerHeader := &tar.Header{
				Name:     layerDir + "/",
				Typeflag: tar.TypeDir,
				Mode:     0755,
			}

			if err := tarWriter.WriteHeader(layerHeader); err != nil {
				return err
			}

			var layerSize int64
			var layerReader io.ReadCloser

			if options != nil && options.UseCompressedLayers {
				layerSize, err = layer.Size()
				if err != nil {
					return err
				}
				layerReader, err = layer.Compressed()
			} else {
				layerSize, err = partial.UncompressedSize(layer)
				if err != nil {
					return err
				}
				layerReader, err = layer.Uncompressed()
			}

			if err != nil {
				return err
			}
			defer layerReader.Close()

			layerTarHeader := &tar.Header{
				Name: layerDir + "/layer.tar",
				Size: layerSize,
				Mode: 0644,
			}

			if err := tarWriter.WriteHeader(layerTarHeader); err != nil {
				return err
			}

			if _, err := io.Copy(tarWriter, layerReader); err != nil {
				return err
			}

			return nil
		}(); err != nil {
			return err
		}

		log.Printf("已处理层 %d/%d", i+1, len(layers))
	}

	singleManifest := map[string]interface{}{
		"Config":   configDigest.String() + ".json",
		"RepoTags": []string{imageRef},
		"Layers": func() []string {
			var layers []string
			for _, digest := range layerDigests {
				layers = append(layers, digest+"/layer.tar")
			}
			return layers
		}(),
	}

	repositories := make(map[string]map[string]string)
	parts := strings.Split(imageRef, ":")
	if len(parts) == 2 {
		repoName := parts[0]
		tag := parts[1]
		repositories[repoName] = map[string]string{tag: configDigest.String()}
	}

	if manifestOut != nil && repositoriesOut != nil {
		*manifestOut = singleManifest
		*repositoriesOut = repositories
		return nil
	}

	manifest := []map[string]interface{}{singleManifest}

	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	manifestHeader := &tar.Header{
		Name: "manifest.json",
		Size: int64(len(manifestData)),
		Mode: 0644,
	}

	if err := tarWriter.WriteHeader(manifestHeader); err != nil {
		return err
	}

	if _, err := tarWriter.Write(manifestData); err != nil {
		return err
	}

	repositoriesData, err := json.Marshal(repositories)
	if err != nil {
		return err
	}

	repositoriesHeader := &tar.Header{
		Name: "repositories",
		Size: int64(len(repositoriesData)),
		Mode: 0644,
	}

	if err := tarWriter.WriteHeader(repositoriesHeader); err != nil {
		return err
	}

	_, err = tarWriter.Write(repositoriesData)
	return err
}

// processImageForBatch 处理镜像的公共逻辑
func (is *ImageStreamer) processImageForBatch(ctx context.Context, img v1.Image, tarWriter *tar.Writer, imageRef string, options *StreamOptions) (map[string]interface{}, map[string]map[string]string, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, fmt.Errorf("获取镜像层失败: %w", err)
	}

	configFile, err := img.ConfigFile()
	if err != nil {
		return nil, nil, fmt.Errorf("获取镜像配置失败: %w", err)
	}

	log.Printf("镜像包含 %d 层", len(layers))

	var manifest map[string]interface{}
	var repositories map[string]map[string]string

	err = is.streamDockerFormatWithReturn(ctx, tarWriter, img, layers, configFile, imageRef, &manifest, &repositories, options)
	if err != nil {
		return nil, nil, err
	}

	return manifest, repositories, nil
}

func (is *ImageStreamer) streamSingleImageForBatch(ctx context.Context, tarWriter *tar.Writer, imageRef string, options *StreamOptions) (map[string]interface{}, map[string]map[string]string, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, nil, fmt.Errorf("解析镜像引用失败: %w", err)
	}

	contextOptions := append(is.remoteOptions, remote.WithContext(ctx))

	desc, err := is.getImageDescriptor(ref, contextOptions)
	if err != nil {
		return nil, nil, fmt.Errorf("获取镜像描述失败: %w", err)
	}

	var img v1.Image

	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		img, err = is.selectPlatformImage(desc, options)
		if err != nil {
			return nil, nil, fmt.Errorf("选择平台镜像失败: %w", err)
		}
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		img, err = desc.Image()
		if err != nil {
			return nil, nil, fmt.Errorf("获取镜像失败: %w", err)
		}
	default:
		img, err = desc.Image()
		if err != nil {
			return nil, nil, fmt.Errorf("获取镜像失败: %w", err)
		}
	}

	return is.processImageForBatch(ctx, img, tarWriter, imageRef, options)
}

// selectPlatformImage 从多架构镜像中选择合适的平台镜像
func (is *ImageStreamer) selectPlatformImage(desc *remote.Descriptor, options *StreamOptions) (v1.Image, error) {
	index, err := desc.ImageIndex()
	if err != nil {
		return nil, fmt.Errorf("获取镜像索引失败: %w", err)
	}

	manifest, err := index.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("获取索引清单失败: %w", err)
	}

	var selectedDesc *v1.Descriptor
	for _, m := range manifest.Manifests {
		if m.Platform == nil {
			continue
		}

		if options.Platform != "" {
			platformParts := strings.Split(options.Platform, "/")
			if len(platformParts) >= 2 {
				targetOS := platformParts[0]
				targetArch := platformParts[1]
				targetVariant := ""
				if len(platformParts) >= 3 {
					targetVariant = platformParts[2]
				}

				if m.Platform.OS == targetOS &&
					m.Platform.Architecture == targetArch &&
					m.Platform.Variant == targetVariant {
					selectedDesc = &m
					break
				}
			}
		} else if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			selectedDesc = &m
			break
		}
	}

	if selectedDesc == nil && len(manifest.Manifests) > 0 {
		selectedDesc = &manifest.Manifests[0]
	}

	if selectedDesc == nil {
		return nil, fmt.Errorf("未找到合适的平台镜像")
	}

	img, err := index.Image(selectedDesc.Digest)
	if err != nil {
		return nil, fmt.Errorf("获取选中镜像失败: %w", err)
	}

	return img, nil
}

var globalImageStreamer *ImageStreamer

// InitImageStreamer 初始化镜像下载器
func InitImageStreamer() {
	globalImageStreamer = NewImageStreamer(nil)
}

// formatPlatformText 格式化平台文本
func formatPlatformText(platform string) string {
	if platform == "" {
		return "自动选择"
	}
	return platform
}

// InitImageTarRoutes 初始化镜像下载路由
func InitImageTarRoutes(router *gin.Engine) {
	imageAPI := router.Group("/api/image")
	{
		imageAPI.GET("/download", handleDirectImageDownload)
		imageAPI.GET("/info", handleImageInfo)
		imageAPI.GET("/batch", handleSimpleBatchDownload)
		imageAPI.POST("/batch", handleSimpleBatchDownload)
	}
}

// resolveImageRef 从 query image 读取镜像引用，避免 path 段用 _ 代替 / 导致下划线歧义。
func resolveImageRef(c *gin.Context) string {
	return strings.TrimSpace(c.Query("image"))
}

// handleDirectImageDownload 处理单镜像下载
func handleDirectImageDownload(c *gin.Context) {
	imageRef := resolveImageRef(c)
	if imageRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少镜像参数"})
		return
	}

	platform := c.Query("platform")
	tag := c.DefaultQuery("tag", "")
	useCompressed := c.DefaultQuery("compressed", "true") == "true"

	if tag != "" && !strings.Contains(imageRef, ":") && !strings.Contains(imageRef, "@") {
		imageRef = imageRef + ":" + tag
	} else if !strings.Contains(imageRef, ":") && !strings.Contains(imageRef, "@") {
		imageRef = imageRef + ":latest"
	}

	if _, err := name.ParseReference(imageRef); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像引用格式错误: " + err.Error()})
		return
	}
	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageRef); !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	if c.Query("mode") == "prepare" {
		userID := getUserID(c)
		contentKey := generateContentFingerprint([]string{imageRef}, platform)

		if !singleImageDebouncer.ShouldAllow(userID, contentKey) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":       "请求过于频繁，请稍后再试",
				"retry_after": 5,
			})
			return
		}

		ip, userAgent := getClientIdentity(c)
		token, err := singleDownloadTokens.create(SingleDownloadRequest{
			Image:               imageRef,
			Platform:            platform,
			UseCompressedLayers: useCompressed,
		}, ip, userAgent)
		if err != nil {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
			return
		}

		q := url.Values{}
		q.Set("image", imageRef)
		q.Set("token", token)
		c.JSON(http.StatusOK, gin.H{"download_url": "/api/image/download?" + q.Encode()})
		return
	}

	token := c.Query("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少下载令牌"})
		return
	}

	ip, userAgent := getClientIdentity(c)
	req, ok := singleDownloadTokens.consume(token, ip, userAgent)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效或过期的下载令牌"})
		return
	}
	if req.Image != imageRef {
		c.JSON(http.StatusBadRequest, gin.H{"error": "下载令牌与镜像不匹配"})
		return
	}
	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(req.Image); !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	options := &StreamOptions{
		Platform:            req.Platform,
		Compression:         false,
		UseCompressedLayers: req.UseCompressedLayers,
	}

	ctx := c.Request.Context()
	log.Printf("下载镜像: %s (平台: %s)", req.Image, formatPlatformText(req.Platform))

	if err := globalImageStreamer.StreamImageToGin(ctx, req.Image, c, options); err != nil {
		writeDownloadError(c, err, "镜像下载失败")
		return
	}
}

// handleSimpleBatchDownload 处理批量下载
func handleSimpleBatchDownload(c *gin.Context) {
	if c.Request.Method == http.MethodGet {
		token := c.Query("token")
		if token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "缺少下载令牌"})
			return
		}

		ip, userAgent := getClientIdentity(c)
		req, ok := batchDownloadTokens.consume(token, ip, userAgent)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效或过期的下载令牌"})
			return
		}

		if len(req.Images) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "镜像列表不能为空"})
			return
		}

		options := &StreamOptions{
			Platform:            req.Platform,
			Compression:         false,
			UseCompressedLayers: req.UseCompressedLayers,
		}

		ctx := c.Request.Context()
		log.Printf("批量下载 %d 个镜像 (平台: %s)", len(req.Images), formatPlatformText(req.Platform))

		filename := fmt.Sprintf("batch_%d_images.tar", len(req.Images))
		// 响应头延迟到首个镜像成功后再发送：若全部失败，仍可返回 JSON 报告
		lazyWriter := &lazyHeaderWriter{
			c:          c,
			filename:   filename,
			compressed: options.Compression,
		}

		report, err := globalImageStreamer.StreamMultipleImages(ctx, req.Images, lazyWriter, options)
		if err != nil {
			writeDownloadError(c, err, "批量镜像下载失败")
			return
		}
		if !lazyWriter.written {
			c.JSON(http.StatusBadGateway, gin.H{
				"error":  "所有镜像均打包失败",
				"report": report,
			})
			return
		}
		if report.FailureCount > 0 {
			log.Printf("批量下载部分失败: 成功 %d，失败 %d，明细见包内 download-report.txt",
				report.SuccessCount, report.FailureCount)
		}
		return
	}

	if c.Query("mode") != "prepare" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只支持prepare模式"})
		return
	}

	var req struct {
		Images              []string `json:"images" binding:"required"`
		Platform            string   `json:"platform"`
		UseCompressedLayers *bool    `json:"useCompressedLayers"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数错误: " + err.Error()})
		return
	}

	if len(req.Images) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像列表不能为空"})
		return
	}
	for _, imageRef := range req.Images {
		if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageRef); !allowed {
			c.JSON(http.StatusForbidden, gin.H{"error": reason})
			return
		}
	}

	for i, imageRef := range req.Images {
		if !strings.Contains(imageRef, ":") && !strings.Contains(imageRef, "@") {
			req.Images[i] = imageRef + ":latest"
		}
	}
	for _, imageRef := range req.Images {
		if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageRef); !allowed {
			c.JSON(http.StatusForbidden, gin.H{"error": reason})
			return
		}
	}

	cfg := config.GetConfig()
	if len(req.Images) > cfg.Download.MaxImages {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("镜像数量超过限制，最大允许: %d", cfg.Download.MaxImages),
		})
		return
	}

	userID := getUserID(c)
	contentKey := generateContentFingerprint(req.Images, req.Platform)

	if !batchImageDebouncer.ShouldAllow(userID, contentKey) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":       "批量下载请求过于频繁，请稍后再试",
			"retry_after": 60,
		})
		return
	}

	useCompressed := true
	if req.UseCompressedLayers != nil {
		useCompressed = *req.UseCompressedLayers
	}

	batchReq := BatchDownloadRequest{
		Images:              req.Images,
		Platform:            req.Platform,
		UseCompressedLayers: useCompressed,
	}

	ip, userAgent := getClientIdentity(c)
	token, err := batchDownloadTokens.create(batchReq, ip, userAgent)
	if err != nil {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"download_url": fmt.Sprintf("/api/image/batch?token=%s", token)})
}

// handleImageInfo 处理镜像信息查询
func handleImageInfo(c *gin.Context) {
	imageRef := resolveImageRef(c)
	if imageRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少镜像参数"})
		return
	}

	tag := c.DefaultQuery("tag", "latest")

	if !strings.Contains(imageRef, ":") && !strings.Contains(imageRef, "@") {
		imageRef = imageRef + ":" + tag
	}

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像引用格式错误: " + err.Error()})
		return
	}
	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageRef); !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": reason})
		return
	}

	ctx := c.Request.Context()
	contextOptions := append(globalImageStreamer.remoteOptions, remote.WithContext(ctx))

	desc, err := globalImageStreamer.getImageDescriptor(ref, contextOptions)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取镜像信息失败: " + err.Error()})
		return
	}

	info := gin.H{
		"name":      ref.String(),
		"mediaType": desc.MediaType,
		"digest":    desc.Digest.String(),
		"size":      desc.Size,
	}

	if desc.MediaType == types.OCIImageIndex || desc.MediaType == types.DockerManifestList {
		index, err := desc.ImageIndex()
		if err == nil {
			manifest, err := index.IndexManifest()
			if err == nil {
				var platforms []string
				for _, m := range manifest.Manifests {
					if m.Platform != nil {
						platforms = append(platforms, m.Platform.OS+"/"+m.Platform.Architecture)
					}
				}
				info["platforms"] = platforms
				info["multiArch"] = true
			}
		}
	} else {
		info["multiArch"] = false
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "data": info})
}

// BatchImageFailure 批量下载中失败的镜像及原因
type BatchImageFailure struct {
	Image string `json:"image"`
	Error string `json:"error"`
}

// BatchReport 批量下载结果报告
type BatchReport struct {
	Platform     string              `json:"platform,omitempty"`
	Total        int                 `json:"total"`
	SuccessCount int                 `json:"success_count"`
	FailureCount int                 `json:"failure_count"`
	Succeeded    []string            `json:"succeeded"`
	Failed       []BatchImageFailure `json:"failed"`
	GeneratedAt  time.Time           `json:"generated_at"`
}

// batchImageTimeout 单个镜像打包超时时间
const batchImageTimeout = 15 * time.Minute

// imageStager 将单个镜像的 docker-save 条目写入独立 tar，便于失败隔离
type imageStager func(ctx context.Context, tarWriter *tar.Writer, imageRef string, options *StreamOptions) (map[string]interface{}, map[string]map[string]string, error)

// StreamMultipleImages 批量下载多个镜像。
// 单个镜像失败不会中断整个批次：每个镜像先暂存到独立临时 tar，
// 成功后才拼接到输出流，失败则跳过并记录；包内附带下载报告。
// 若全部失败，不会向 writer 写入任何数据，由调用方返回 JSON 错误。
func (is *ImageStreamer) StreamMultipleImages(ctx context.Context, imageRefs []string, writer io.Writer, options *StreamOptions) (*BatchReport, error) {
	return is.streamMultipleImagesWithStager(ctx, imageRefs, writer, options, is.streamSingleImageForBatch)
}

func (is *ImageStreamer) streamMultipleImagesWithStager(ctx context.Context, imageRefs []string, writer io.Writer, options *StreamOptions, stage imageStager) (report *BatchReport, err error) {
	if options == nil {
		options = &StreamOptions{UseCompressedLayers: true}
	}

	report = &BatchReport{
		Platform:    options.Platform,
		Total:       len(imageRefs),
		Succeeded:   []string{},
		Failed:      []BatchImageFailure{},
		GeneratedAt: time.Now(),
	}

	finalWriter := writer
	var gzWriter *lazyGzipWriter
	if options.Compression {
		gzWriter = &lazyGzipWriter{parent: writer}
		finalWriter = gzWriter
		defer func() {
			if closeErr := gzWriter.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}()
	}

	allManifests := []map[string]interface{}{}
	allRepositories := make(map[string]map[string]string)

	for i, imageRef := range imageRefs {
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		default:
		}

		log.Printf("处理镜像 %d/%d: %s", i+1, len(imageRefs), imageRef)

		manifest, repositories, stageErr := is.stageOneImage(ctx, imageRef, options, finalWriter, stage)
		if stageErr != nil {
			log.Printf("镜像 %s 打包失败，已跳过: %v", imageRef, stageErr)
			report.Failed = append(report.Failed, BatchImageFailure{
				Image: imageRef,
				Error: stageErr.Error(),
			})
			continue
		}

		report.Succeeded = append(report.Succeeded, imageRef)
		allManifests = append(allManifests, manifest)
		for repo, tags := range repositories {
			if allRepositories[repo] == nil {
				allRepositories[repo] = make(map[string]string)
			}
			for tag, digest := range tags {
				allRepositories[repo][tag] = digest
			}
		}
	}

	report.SuccessCount = len(report.Succeeded)
	report.FailureCount = len(report.Failed)

	// 全部失败时不写出 tar，由调用方向客户端返回 JSON 报告
	if report.SuccessCount == 0 {
		return report, nil
	}

	tarWriter := tar.NewWriter(finalWriter)

	if err := writeTarJSON(tarWriter, "manifest.json", allManifests); err != nil {
		return report, fmt.Errorf("写入manifest失败: %w", err)
	}
	if err := writeTarJSON(tarWriter, "repositories", allRepositories); err != nil {
		return report, fmt.Errorf("写入repositories失败: %w", err)
	}
	if err := writeTarJSON(tarWriter, "download-report.json", report); err != nil {
		return report, fmt.Errorf("写入下载报告失败: %w", err)
	}
	if err := writeTarText(tarWriter, "download-report.txt", renderBatchReportText(report)); err != nil {
		return report, fmt.Errorf("写入下载报告失败: %w", err)
	}

	if err := tarWriter.Close(); err != nil {
		return report, fmt.Errorf("关闭tar失败: %w", err)
	}

	log.Printf("批量下载完成: 共 %d 个，成功 %d 个，失败 %d 个",
		report.Total, report.SuccessCount, report.FailureCount)
	return report, nil
}

// lazyGzipWriter 延迟初始化 gzip：首次写入时才写 gzip 头尾，
// 保证全部镜像失败、无任何字节产出时不会污染下游输出。
type lazyGzipWriter struct {
	parent io.Writer
	gz     *gzip.Writer
}

func (w *lazyGzipWriter) Write(p []byte) (int, error) {
	if w.gz == nil && len(p) > 0 {
		w.gz = gzip.NewWriter(w.parent)
	}
	if w.gz == nil {
		return len(p), nil
	}
	return w.gz.Write(p)
}

func (w *lazyGzipWriter) Close() error {
	if w.gz == nil {
		return nil
	}
	return w.gz.Close()
}

// stageOneImage 将单个镜像暂存到临时文件，成功后把完整 tar 条目拼接到输出流。
// 拼接的是暂存 tar 中除结尾零块外的原始条目，多个镜像条目首尾相接仍为合法 tar。
func (is *ImageStreamer) stageOneImage(ctx context.Context, imageRef string, options *StreamOptions, output io.Writer, stage imageStager) (map[string]interface{}, map[string]map[string]string, error) {
	tmp, err := os.CreateTemp("", "hubproxy-batch-*.tar")
	if err != nil {
		return nil, nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()

	stageWriter := tar.NewWriter(tmp)

	timeoutCtx, cancel := context.WithTimeout(ctx, batchImageTimeout)
	manifest, repositories, stageErr := stage(timeoutCtx, stageWriter, imageRef, options)
	cancel()

	if stageErr != nil {
		_ = stageWriter.Close()
		return nil, nil, stageErr
	}
	if manifest == nil {
		_ = stageWriter.Close()
		return nil, nil, fmt.Errorf("manifest数据为空")
	}

	// Flush 先把最后一个条目的填充块落盘，此时文件长度恰好是全部条目字节数；
	// 随后的 Close 只会追加 1024 字节的 tar 结尾零块，拼接时需排除。
	flushErr := stageWriter.Flush()
	entrySize, seekErr := tmp.Seek(0, io.SeekCurrent)
	closeErr := stageWriter.Close()

	if flushErr != nil {
		return nil, nil, fmt.Errorf("刷新临时tar失败: %w", flushErr)
	}
	if seekErr != nil {
		return nil, nil, fmt.Errorf("记录临时文件大小失败: %w", seekErr)
	}
	if closeErr != nil {
		return nil, nil, fmt.Errorf("关闭临时tar失败: %w", closeErr)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("回读临时文件失败: %w", err)
	}
	if _, err := io.CopyN(output, tmp, entrySize); err != nil {
		return nil, nil, fmt.Errorf("写入输出流失败: %w", err)
	}

	return manifest, repositories, nil
}

// writeTarJSON 向 tar 写入一个 JSON 文件条目
func writeTarJSON(tarWriter *tar.Writer, name string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeTarText(tarWriter, name, string(data))
}

// writeTarText 向 tar 写入一个文本文件条目
func writeTarText(tarWriter *tar.Writer, name, content string) error {
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: name,
		Size: int64(len(content)),
		Mode: 0644,
	}); err != nil {
		return err
	}
	_, err := io.WriteString(tarWriter, content)
	return err
}

// renderBatchReportText 生成人类可读的批量下载报告
func renderBatchReportText(report *BatchReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HubProxy 批量下载报告\n")
	fmt.Fprintf(&b, "生成时间: %s\n", report.GeneratedAt.Format("2006-01-02 15:04:05"))
	if report.Platform != "" {
		fmt.Fprintf(&b, "目标平台: %s\n", report.Platform)
	}
	fmt.Fprintf(&b, "总数: %d，成功: %d，失败: %d\n", report.Total, report.SuccessCount, report.FailureCount)

	if len(report.Succeeded) > 0 {
		b.WriteString("\n成功:\n")
		for _, image := range report.Succeeded {
			fmt.Fprintf(&b, "  [OK]   %s\n", image)
		}
	}
	if len(report.Failed) > 0 {
		b.WriteString("\n失败:\n")
		for _, item := range report.Failed {
			fmt.Fprintf(&b, "  [FAIL] %s\n", item.Image)
			fmt.Fprintf(&b, "         原因: %s\n", item.Error)
		}
	}
	return b.String()
}
