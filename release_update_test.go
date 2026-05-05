package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestSelectChecksumAsset(t *testing.T) {
	release := githubRelease{Assets: []githubReleaseAsset{
		{Name: "codex-session-search_linux_amd64", BrowserDownloadURL: "https://example.invalid/linux"},
		{Name: updateChecksumName, BrowserDownloadURL: "https://example.invalid/checksums"},
	}}
	asset, ok := selectChecksumAsset(release)
	if !ok {
		t.Fatal("checksum asset was not selected")
	}
	if asset.BrowserDownloadURL != "https://example.invalid/checksums" {
		t.Fatalf("checksum asset URL = %q", asset.BrowserDownloadURL)
	}
}

func TestChecksumForAsset(t *testing.T) {
	checksums := []byte(strings.Join([]string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  codex-session-search_linux_amd64",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  *codex-session-search_darwin_arm64",
	}, "\n"))
	got, err := checksumForAsset(checksums, "codex-session-search_darwin_arm64")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("b", sha256.Size*2)
	if got != want {
		t.Fatalf("checksum = %q, want %q", got, want)
	}
}

func TestVerifyDownloadedChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset")
	data := []byte("release asset")
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := verifyDownloadedChecksum(path, "asset", sum); err != nil {
		t.Fatal(err)
	}
	if err := verifyDownloadedChecksum(path, "asset", strings.Repeat("0", sha256.Size*2)); err == nil {
		t.Fatal("expected checksum mismatch")
	}
}

func TestValidateVersionOutput(t *testing.T) {
	if err := validateVersionOutput("codex-session-search v0.3.0 (abc, built now)", "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if err := validateVersionOutput("codex-session-search 0.3.0 (abc, built now)", "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if err := validateVersionOutput("codex-session-search v0.2.9 (abc, built now)", "v0.3.0"); err == nil {
		t.Fatal("expected version mismatch")
	}
	if err := validateVersionOutput("unexpected output", "v0.3.0"); err == nil {
		t.Fatal("expected unexpected output error")
	}
}

func TestShouldSkipRecentFailedUpdate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := saveUpdateState(updateState{
		FailedTagName: "v0.3.0",
		Failure:       "checksum mismatch",
		FailedAt:      time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	skipped, failure := shouldSkipRecentFailedUpdate("0.3.0")
	if !skipped {
		t.Fatal("expected recent failed update to be skipped")
	}
	if failure != "checksum mismatch" {
		t.Fatalf("failure = %q", failure)
	}

	if err := saveUpdateState(updateState{
		FailedTagName: "v0.3.0",
		Failure:       "checksum mismatch",
		FailedAt:      time.Now().Add(-updateFailureCooldown - time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	skipped, _ = shouldSkipRecentFailedUpdate("v0.3.0")
	if skipped {
		t.Fatal("expired failed update should not be skipped")
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

func TestUpdateNoticeLinesShowDaemonRestartResult(t *testing.T) {
	notice := updateNotice{
		Kind:            updateNoticeUpdated,
		LatestVersion:   "v0.3.0",
		DaemonRestarted: true,
	}
	lines := updateNoticeLines(notice, 80, 3)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "Daemon restarted.") {
		t.Fatalf("missing daemon restart status: %q", joined)
	}

	notice = updateNotice{
		Kind:               updateNoticeUpdated,
		LatestVersion:      "v0.3.0",
		DaemonRestartError: "restart failed",
	}
	lines = updateNoticeLines(notice, 80, 3)
	joined = strings.Join(lines, "\n")
	if !strings.Contains(joined, "Daemon restart: restart failed") {
		t.Fatalf("missing daemon restart error: %q", joined)
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
