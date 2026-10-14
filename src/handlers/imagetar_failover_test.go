package handlers

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"hubproxy/utils"
)

// TestImageTarStreamFailover 离线 tar 下载：主上游不可用时，manifest、config 与 layer
// 均从备用上游拉取，生成的 tar 内容完整。
func TestImageTarStreamFailover(t *testing.T) {
	// 构造真实的 schema2 镜像：config + 一个 gzip 层
	rawLayer := []byte("hello-failover-layer")
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, _ = gw.Write(rawLayer)
	_ = gw.Close()
	layerBlob := gzBuf.Bytes()
	layerDigest := "sha256:" + sha256Hex(layerBlob)
	diffID := "sha256:" + sha256Hex(rawLayer)

	config := map[string]interface{}{
		"architecture": "amd64",
		"os":           "linux",
		"config":       map[string]interface{}{},
		"rootfs": map[string]interface{}{
			"type":     "layers",
			"diff_ids": []string{diffID},
		},
	}
	configBlob, _ := json.Marshal(config)
	configDigest := "sha256:" + sha256Hex(configBlob)

	manifest := map[string]interface{}{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]interface{}{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"size":      len(configBlob),
			"digest":    configDigest,
		},
		"layers": []map[string]interface{}{{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"size":      len(layerBlob),
			"digest":    layerDigest,
		}},
	}
	manifestBlob, _ := json.Marshal(manifest)

	alive := newRegistryServer(t, manifestBlob,
		"application/vnd.docker.distribution.manifest.v2+json",
		map[string][]byte{configDigest: configBlob, layerDigest: layerBlob}, false)
	defer alive.Close()

	newFailoverTestRouter(t, deadLocalhost(t), alive.URL[len("http://"):])

	streamer := NewImageStreamer(&ImageStreamerConfig{Concurrency: 1})

	var out bytes.Buffer
	err := streamer.StreamImageToWriter(context.Background(), "nginx:latest", &out, &StreamOptions{
		UseCompressedLayers: true,
	})
	if err != nil {
		t.Fatalf("tar 流式下载失败: %v", err)
	}

	entries := map[string][]byte{}
	tr := tar.NewReader(&out)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读取 tar 失败: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("读取 tar 条目 %s 失败: %v", hdr.Name, err)
		}
		entries[hdr.Name] = data
	}

	if !bytes.Equal(entries[configDigest+".json"], configBlob) {
		t.Fatal("config blob 内容不一致")
	}
	if !bytes.Equal(entries[layerDigest+"/layer.tar"], layerBlob) {
		t.Fatal("layer blob 内容不一致")
	}

	var tarManifest []map[string]interface{}
	if err := json.Unmarshal(entries["manifest.json"], &tarManifest); err != nil {
		t.Fatalf("manifest.json 解析失败: %v", err)
	}
	if len(tarManifest) != 1 {
		t.Fatalf("manifest.json 条数 = %d", len(tarManifest))
	}
	layers, _ := tarManifest[0]["Layers"].([]interface{})
	if len(layers) != 1 || layers[0] != layerDigest+"/layer.tar" {
		t.Fatalf("manifest.json layers 异常: %v", layers)
	}
	repoTags, _ := tarManifest[0]["RepoTags"].([]interface{})
	if len(repoTags) == 0 || !strings.Contains(repoTags[0].(string), "nginx") {
		t.Fatalf("manifest.json RepoTags 异常: %v", repoTags)
	}

	// 主上游在整个过程中被标记不健康，备用上游健康
	status := map[string]utils.HostStatus{}
	for _, s := range utils.GlobalUpstreamManager.PoolStatus(utils.DockerHubPool) {
		status[s.Host] = s
	}
	for host, s := range status {
		aliveHost := alive.URL[len("http://"):]
		want := host == aliveHost
		if s.Healthy != want {
			t.Fatalf("上游 %s healthy=%v, want %v", host, s.Healthy, want)
		}
	}
}
