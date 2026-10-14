package handlers

import "testing"

func TestCheckGitHubURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		user string
		repo string
	}{
		{"release", "https://github.com/user/repo/releases/download/v1/file.tar.gz", "user", "repo"},
		{"raw", "https://raw.githubusercontent.com/user/repo/main/file.sh", "user", "repo"},
		{"api", "https://api.github.com/repos/user/repo/releases/latest", "user", "repo"},
		{"huggingface", "https://huggingface.co/user/model/resolve/main/file", "user", "model/resolve/main/file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckGitHubURL(tt.url)
			if len(got) < 2 || got[0] != tt.user || got[1] != tt.repo {
				t.Fatalf("CheckGitHubURL(%q) = %#v", tt.url, got)
			}
		})
	}
}

func TestIsHeavyDownloadURL(t *testing.T) {
	heavy := []string{
		"https://github.com/user/repo/releases/download/v1/file.tar.gz",
		"https://github.com/user/repo/archive/refs/tags/v1.tar.gz",
		"https://huggingface.co/user/model/resolve/main/model.bin",
		"https://huggingface.co/spaces/user/app/resolve/main/data.parquet",
		"https://cdn-lfs.hf.co/user/repo/abcdef?response-content-disposition=attachment",
	}
	for _, u := range heavy {
		if !isHeavyDownloadURL(u) {
			t.Fatalf("期望判定为大文件下载: %s", u)
		}
	}

	light := []string{
		"https://raw.githubusercontent.com/user/repo/main/install.sh",
		"https://github.com/user/repo/blob/main/README.md",
		"https://api.github.com/repos/user/repo/releases/latest",
		"https://github.com/user/repo/info/refs?service=git-upload-pack",
		"https://huggingface.co/api/models/user/model",
	}
	for _, u := range light {
		if isHeavyDownloadURL(u) {
			t.Fatalf("不应判定为大文件下载: %s", u)
		}
	}
}

func TestCheckGitHubURLRejectsOtherHosts(t *testing.T) {
	if got := CheckGitHubURL("https://example.com/user/repo/file"); got != nil {
		t.Fatalf("unexpected match: %#v", got)
	}
	if got := CheckGitHubURL("https://download.docker.com/linux/static/stable/x86_64/docker.tgz"); got != nil {
		t.Fatalf("download.docker.com should be rejected: %#v", got)
	}
}
