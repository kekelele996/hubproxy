package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"hubproxy/config"
	"hubproxy/utils"
)

// RegistryDetector Registry检测器
type RegistryDetector struct{}

// detectRegistryDomain 检测Registry域名并返回域名和剩余路径
func (rd *RegistryDetector) detectRegistryDomain(c *gin.Context, path string) (string, string) {
	cfg := config.GetConfig()

	// 兼容Containerd的ns参数
	if ns := c.Query("ns"); ns != "" {
		if mapping, exists := cfg.Registries[ns]; exists && mapping.Enabled {
			return ns, path
		}
	}

	for domain := range cfg.Registries {
		if strings.HasPrefix(path, domain+"/") {
			remainingPath := strings.TrimPrefix(path, domain+"/")
			return domain, remainingPath
		}
	}

	return "", path
}

// isRegistryEnabled 检查Registry是否启用
func (rd *RegistryDetector) isRegistryEnabled(domain string) bool {
	cfg := config.GetConfig()
	if mapping, exists := cfg.Registries[domain]; exists {
		return mapping.Enabled
	}
	return false
}

// getRegistryMapping 获取Registry映射配置
func (rd *RegistryDetector) getRegistryMapping(domain string) (config.RegistryMapping, bool) {
	cfg := config.GetConfig()
	mapping, exists := cfg.Registries[domain]
	return mapping, exists && mapping.Enabled
}

var registryDetector = &RegistryDetector{}

// ProxyDockerRegistryGin 标准Docker Registry API v2代理
func ProxyDockerRegistryGin(c *gin.Context) {
	path := c.Request.URL.Path

	if path == "/v2/" {
		c.JSON(http.StatusOK, gin.H{})
		return
	}

	if strings.HasPrefix(path, "/v2/") {
		handleRegistryRequest(c, path)
	} else {
		c.String(http.StatusNotFound, "Docker Registry API v2 only")
	}
}

// handleRegistryRequest 处理Registry请求
func handleRegistryRequest(c *gin.Context, path string) {
	pathWithoutV2 := strings.TrimPrefix(path, "/v2/")

	if registryDomain, remainingPath := registryDetector.detectRegistryDomain(c, pathWithoutV2); registryDomain != "" {
		if registryDetector.isRegistryEnabled(registryDomain) {
			handleConfiguredRegistryRequest(c, registryDomain, remainingPath)
			return
		}
	}

	imageName, apiType, reference := parseRegistryPath(pathWithoutV2)
	if imageName == "" || apiType == "" {
		c.String(http.StatusBadRequest, "Invalid path format")
		return
	}

	if !strings.Contains(imageName, "/") {
		imageName = "library/" + imageName
	}

	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(imageName); !allowed {
		fmt.Printf("Docker镜像 %s 访问被拒绝: %s\n", imageName, reason)
		c.String(http.StatusForbidden, "镜像访问被限制")
		return
	}

	cfg := config.GetConfig()
	loc := locatorForRegistry(utils.DockerHubPool, cfg.Upstream.DockerHubUpstreams, imageName)
	dispatchRegistryRequest(c, loc, imageName, apiType, reference)
}

// handleConfiguredRegistryRequest 处理已配置第三方Registry的请求
func handleConfiguredRegistryRequest(c *gin.Context, registryDomain, remainingPath string) {
	mapping, exists := registryDetector.getRegistryMapping(registryDomain)
	if !exists {
		c.String(http.StatusBadRequest, "Registry not configured")
		return
	}

	imageName, apiType, reference := parseRegistryPath(remainingPath)
	if imageName == "" || apiType == "" {
		c.String(http.StatusBadRequest, "Invalid path format")
		return
	}

	fullImageName := registryDomain + "/" + imageName
	if allowed, reason := utils.GlobalAccessController.CheckDockerAccess(fullImageName); !allowed {
		fmt.Printf("镜像 %s 访问被拒绝: %s\n", fullImageName, reason)
		c.String(http.StatusForbidden, "镜像访问被限制")
		return
	}

	loc := locatorForRegistry(registryDomain, mapping.UpstreamList(), imageName)
	dispatchRegistryRequest(c, loc, imageName, apiType, reference)
}

// dispatchRegistryRequest 按 API 类型分发请求
func dispatchRegistryRequest(c *gin.Context, loc refLocator, imageName, apiType, reference string) {
	switch apiType {
	case "manifests":
		handleManifestFailover(c, loc, reference)
	case "blobs":
		handleBlobFailover(c, loc, reference)
	case "tags":
		handleTagsFailover(c, loc)
	default:
		c.String(http.StatusNotFound, "API endpoint not found")
	}
}

// parseRegistryPath 解析Registry路径
func parseRegistryPath(path string) (imageName, apiType, reference string) {
	if idx := strings.Index(path, "/manifests/"); idx != -1 {
		imageName = path[:idx]
		apiType = "manifests"
		reference = path[idx+len("/manifests/"):]
		return
	}

	if idx := strings.Index(path, "/blobs/"); idx != -1 {
		imageName = path[:idx]
		apiType = "blobs"
		reference = path[idx+len("/blobs/"):]
		return
	}

	if idx := strings.Index(path, "/tags/list"); idx != -1 {
		imageName = path[:idx]
		apiType = "tags"
		reference = "list"
		return
	}

	return "", "", ""
}

// logicalImageRef 返回与具体上游无关的逻辑镜像名，用于缓存 key
func logicalImageRef(loc refLocator) string {
	return loc.pool + "/" + loc.repo
}

// writeUpstreamError 统一映射故障切换后的错误：所有上游不可用返回 502，其余返回 404。
func writeUpstreamError(c *gin.Context, err error, notFoundMessage string) {
	if errors.Is(err, errAllUpstreamsUnavailable) {
		fmt.Printf("所有上游均不可用: %v\n", err)
		c.String(http.StatusBadGateway, "所有上游暂时不可用，请稍后重试")
		return
	}
	fmt.Printf("上游请求失败: %v\n", err)
	c.String(http.StatusNotFound, notFoundMessage)
}

// handleManifestFailover 处理manifest请求（自动故障切换）
func handleManifestFailover(c *gin.Context, loc refLocator, reference string) {
	cacheRef := logicalImageRef(loc)

	if utils.IsCacheEnabled() && c.Request.Method == http.MethodGet {
		cacheKey := utils.BuildManifestCacheKey(cacheRef, reference)

		if cachedItem := utils.GlobalCache.Get(cacheKey); cachedItem != nil {
			utils.WriteCachedResponse(c, cachedItem)
			return
		}
	}

	options := baseRemoteOptions()

	if c.Request.Method == http.MethodHead {
		desc, err := remoteHeadWithFailover(loc, reference, options)
		if err != nil {
			writeUpstreamError(c, err, "Manifest not found")
			return
		}

		c.Header("Content-Type", string(desc.MediaType))
		c.Header("Docker-Content-Digest", desc.Digest.String())
		c.Header("Content-Length", fmt.Sprintf("%d", desc.Size))
		c.Status(http.StatusOK)
		return
	}

	desc, err := remoteGetWithFailover(loc, reference, options)
	if err != nil {
		writeUpstreamError(c, err, "Manifest not found")
		return
	}

	headers := map[string]string{
		"Docker-Content-Digest": desc.Digest.String(),
		"Content-Length":        fmt.Sprintf("%d", len(desc.Manifest)),
	}

	if utils.IsCacheEnabled() {
		cacheKey := utils.BuildManifestCacheKey(cacheRef, reference)
		ttl := utils.GetManifestTTL(reference)
		utils.GlobalCache.Set(cacheKey, desc.Manifest, string(desc.MediaType), headers, ttl)
	}

	c.Header("Content-Type", string(desc.MediaType))
	for key, value := range headers {
		c.Header(key, value)
	}

	c.Data(http.StatusOK, string(desc.MediaType), desc.Manifest)
}

// handleBlobFailover 处理blob（layer）请求，首字节前自动切换上游
func handleBlobFailover(c *gin.Context, loc refLocator, digest string) {
	// 提前校验 digest 引用格式，格式错误直接返回 400
	if _, err := loc.rebasedDigestRef(loc.hosts[0], digest); err != nil {
		c.String(http.StatusBadRequest, "Invalid digest reference")
		return
	}

	reader, err := openBlobWithFailover(loc, digest, true, baseRemoteOptions())
	if err != nil {
		writeUpstreamError(c, err, "Layer not found")
		return
	}
	defer reader.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Length", fmt.Sprintf("%d", reader.Size()))
	c.Header("Docker-Content-Digest", digest)

	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, reader); err != nil {
		fmt.Printf("复制layer内容失败: %v\n", err)
	}
}

// handleTagsFailover 处理tags列表请求（自动故障切换）
func handleTagsFailover(c *gin.Context, loc refLocator) {
	tags, err := remoteListWithFailover(loc, baseRemoteOptions())
	if err != nil {
		writeUpstreamError(c, err, "Tags not found")
		return
	}

	response := map[string]interface{}{
		"name": loc.repo,
		"tags": tags,
	}

	c.JSON(http.StatusOK, response)
}

// ProxyDockerAuthGin Docker认证代理
func ProxyDockerAuthGin(c *gin.Context) {
	if utils.IsTokenCacheEnabled() {
		proxyDockerAuthWithCache(c)
	} else {
		proxyDockerAuthOriginal(c)
	}
}

// proxyDockerAuthWithCache 带缓存的认证代理
func proxyDockerAuthWithCache(c *gin.Context) {
	cacheKey := utils.BuildTokenCacheKey(c.Request.URL.RawQuery)

	if cachedToken := utils.GlobalCache.GetToken(cacheKey); cachedToken != "" {
		utils.WriteTokenResponse(c, cachedToken)
		return
	}

	recorder := &ResponseRecorder{
		ResponseWriter: c.Writer,
		statusCode:     200,
	}
	c.Writer = recorder

	proxyDockerAuthOriginal(c)

	if recorder.statusCode == 200 && len(recorder.body) > 0 {
		ttl := utils.ExtractTTLFromResponse(recorder.body)
		utils.GlobalCache.SetToken(cacheKey, string(recorder.body), ttl)
	}

	c.Writer = recorder.ResponseWriter
	c.Data(recorder.statusCode, "application/json", recorder.body)
}

// ResponseRecorder HTTP响应记录器
type ResponseRecorder struct {
	gin.ResponseWriter
	statusCode int
	body       []byte
}

func (r *ResponseRecorder) WriteHeader(code int) {
	r.statusCode = code
}

func (r *ResponseRecorder) Write(data []byte) (int, error) {
	r.body = append(r.body, data...)
	return len(data), nil
}

func proxyDockerAuthOriginal(c *gin.Context) {
	authURL := buildDockerAuthURL(c)

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: utils.GetGlobalHTTPClient().Transport,
	}

	req, err := http.NewRequestWithContext(
		context.Background(),
		c.Request.Method,
		authURL,
		c.Request.Body,
	)
	if err != nil {
		c.String(http.StatusInternalServerError, "Failed to create request")
		return
	}

	for key, values := range c.Request.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		c.String(http.StatusBadGateway, "Auth request failed")
		return
	}
	defer resp.Body.Close()

	proxyHost := c.Request.Host
	if proxyHost == "" {
		cfg := config.GetConfig()
		proxyHost = fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
		if cfg.Server.Host == "0.0.0.0" {
			proxyHost = fmt.Sprintf("localhost:%d", cfg.Server.Port)
		}
	}

	for key, values := range resp.Header {
		for _, value := range values {
			if key == "Www-Authenticate" {
				value = rewriteAuthHeader(value, proxyHost)
			}
			c.Header(key, value)
		}
	}

	c.Status(resp.StatusCode)
	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
		fmt.Printf("复制认证响应失败: %v\n", err)
	}
}

// buildDockerAuthURL 根据 token 请求的 service 参数选择上游认证地址。
// AuthHost 已包含路径（如 ghcr.io/token、quay.io/v2/auth），不再拼接本机 Path，避免 /token/token。
func buildDockerAuthURL(c *gin.Context) string {
	authHost := resolveAuthHost(c.Query("service"))
	var authURL string
	if authHost != "" {
		authURL = "https://" + authHost
	} else {
		authURL = "https://auth.docker.io" + c.Request.URL.Path
	}
	if c.Request.URL.RawQuery != "" {
		authURL += "?" + c.Request.URL.RawQuery
	}
	return authURL
}

// resolveAuthHost 用 service 匹配已启用 Registry 的 AuthHost；Docker Hub 返回空串走默认路径。
func resolveAuthHost(service string) string {
	if service == "" || service == "registry.docker.io" || service == "docker.io" {
		return ""
	}

	cfg := config.GetConfig()
	for domain, mapping := range cfg.Registries {
		if !mapping.Enabled || mapping.AuthHost == "" {
			continue
		}
		if service == domain || service == mapping.Upstream {
			return mapping.AuthHost
		}
	}
	return ""
}

// rewriteAuthHeader 将上游认证 realm 统一改写到本机 /token，避免 quay 等变成 /v2/auth 误入 Registry 路由。
func rewriteAuthHeader(authHeader, proxyHost string) string {
	proxyToken := "http://" + proxyHost + "/token"

	cfg := config.GetConfig()
	for _, mapping := range cfg.Registries {
		if mapping.AuthHost == "" {
			continue
		}
		authHeader = strings.ReplaceAll(authHeader, "https://"+mapping.AuthHost, proxyToken)
	}
	authHeader = strings.ReplaceAll(authHeader, "https://auth.docker.io/token", proxyToken)
	authHeader = strings.ReplaceAll(authHeader, "https://auth.docker.io", "http://"+proxyHost)

	return authHeader
}
