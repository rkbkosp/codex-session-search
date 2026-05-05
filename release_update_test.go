package main

import (
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		current string
		latest  string
		want    int
	}{
		{current: "0.1.0", latest: "0.1.0", want: 0},
		{current: "0.1.0", latest: "0.1.1", want: -1},
		{current: "v0.2.0", latest: "0.1.9", want: 1},
		{current: "1.0.0-rc.1", latest: "1.0.0", want: -1},
		{current: "1.0.0", latest: "1.0.0-rc.1", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.current+"_"+tt.latest, func(t *testing.T) {
			got := compareVersions(normalizedVersion(tt.current), normalizedVersion(tt.latest))
			switch {
			case got < 0 && tt.want >= 0:
				t.Fatalf("compareVersions() = %d, want %d", got, tt.want)
			case got > 0 && tt.want <= 0:
				t.Fatalf("compareVersions() = %d, want %d", got, tt.want)
			case got == 0 && tt.want != 0:
				t.Fatalf("compareVersions() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestReleaseAssetName(t *testing.T) {
	if got := releaseAssetName("darwin", "arm64"); got != "codex-session-search_darwin_arm64" {
		t.Fatalf("darwin asset = %q", got)
	}
	if got := releaseAssetName("windows", "amd64"); got != "codex-session-search_windows_amd64.exe" {
		t.Fatalf("windows asset = %q", got)
	}
}

func TestSelectReleaseAsset(t *testing.T) {
	release := githubRelease{Assets: []githubReleaseAsset{
		{Name: "codex-session-search_linux_amd64", BrowserDownloadURL: "https://example.invalid/linux"},
		{Name: "codex-session-search_darwin_arm64", BrowserDownloadURL: "https://example.invalid/darwin"},
	}}
	asset, ok := selectReleaseAsset(release, "darwin", "arm64")
	if !ok {
		t.Fatal("darwin arm64 asset was not selected")
	}
	if asset.BrowserDownloadURL != "https://example.invalid/darwin" {
		t.Fatalf("asset URL = %q", asset.BrowserDownloadURL)
	}
}

func TestUpdateNoticeLinesUseReleaseNotes(t *testing.T) {
	notice := updateNotice{
		Kind:          updateNoticeUpdated,
		LatestVersion: "v0.2.0",
		ReleaseNotes:  "## Changes\n\n- feat: add release workflow (abc1234)\n- fix: keep footer compact (def5678)",
	}
	lines := updateNoticeLines(notice, 80, 4)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Updated to v0.2.0") {
		t.Fatalf("missing update summary: %q", joined)
	}
	if !strings.Contains(joined, "feat: add release workflow") {
		t.Fatalf("missing release note: %q", joined)
	}
	if strings.Contains(joined, "## Changes") {
		t.Fatalf("included markdown heading: %q", joined)
	}
}

func TestUpdateFailureNoticeShowsPromptAndReason(t *testing.T) {
	notice := updateNotice{
		Kind:          updateNoticeFailed,
		LatestVersion: "v0.3.0",
		Error:         "permission denied",
	}
	lines := updateNoticeLines(notice, 80, 3)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Update available: v0.3.0") {
		t.Fatalf("missing update prompt: %q", joined)
	}
	if !strings.Contains(joined, "Auto update: permission denied") {
		t.Fatalf("missing failure reason: %q", joined)
	}
}
