package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	updateCheckTimeout    = 8 * time.Second
	updateDownloadTimeout = 90 * time.Second
	updateStateFileName   = "update-state.json"
)

var semverPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

type updateNoticeKind string

const (
	updateNoticeNone      updateNoticeKind = ""
	updateNoticeCurrent   updateNoticeKind = "current"
	updateNoticeAvailable updateNoticeKind = "available"
	updateNoticeUpdated   updateNoticeKind = "updated"
	updateNoticeFailed    updateNoticeKind = "failed"
)

type updateNotice struct {
	Kind           updateNoticeKind
	CurrentVersion string
	LatestVersion  string
	ReleaseURL     string
	ReleaseNotes   string
	Error          string
}

type githubRelease struct {
	TagName     string               `json:"tag_name"`
	Name        string               `json:"name"`
	Body        string               `json:"body"`
	HTMLURL     string               `json:"html_url"`
	Prerelease  bool                 `json:"prerelease"`
	Draft       bool                 `json:"draft"`
	PublishedAt string               `json:"published_at"`
	Assets      []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	ContentType        string `json:"content_type"`
	Size               int64  `json:"size"`
}

type updateState struct {
	TagName    string `json:"tag_name"`
	Version    string `json:"version"`
	Notes      string `json:"notes,omitempty"`
	ReleaseURL string `json:"release_url,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	Shown      bool   `json:"shown,omitempty"`
}

func updateStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "codex-session-search", updateStateFileName), nil
}

func loadUpdateState() (updateState, error) {
	path, err := updateStatePath()
	if err != nil {
		return updateState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return updateState{}, nil
		}
		return updateState{}, err
	}
	var state updateState
	if err := json.Unmarshal(data, &state); err != nil {
		return updateState{}, err
	}
	return state, nil
}

func saveUpdateState(state updateState) error {
	path, err := updateStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSONFileAtomic(path, state)
}

func startupUpdateNotice(currentVersion string) (updateNotice, error) {
	state, err := loadUpdateState()
	if err != nil {
		return updateNotice{}, err
	}
	if state.Version == "" || state.Version != normalizedVersion(currentVersion) {
		return updateNotice{}, nil
	}
	if state.TagName == "" {
		return updateNotice{}, nil
	}
	if state.Shown {
		return updateNotice{}, nil
	}
	state.Shown = true
	_ = saveUpdateState(state)
	return updateNotice{
		Kind:           updateNoticeUpdated,
		CurrentVersion: currentVersion,
		LatestVersion:  state.TagName,
		ReleaseURL:     state.ReleaseURL,
		ReleaseNotes:   state.Notes,
	}, nil
}

func checkForUpdate(ctx context.Context, repo string) (updateNotice, error) {
	latest, err := fetchLatestGitHubRelease(ctx, repo)
	if err != nil {
		return updateNotice{}, err
	}
	current := normalizedVersion(version)
	latestVersion := normalizedVersion(latest.TagName)
	if latestVersion == "" {
		return updateNotice{}, errors.New("latest release did not include a tag")
	}
	if !isDevVersion(current) && compareVersions(current, latestVersion) >= 0 {
		return updateNotice{
			Kind:           updateNoticeCurrent,
			CurrentVersion: version,
			LatestVersion:  latest.TagName,
			ReleaseURL:     latest.HTMLURL,
			ReleaseNotes:   latest.Body,
		}, nil
	}
	return updateNotice{
		Kind:           updateNoticeAvailable,
		CurrentVersion: version,
		LatestVersion:  latest.TagName,
		ReleaseURL:     latest.HTMLURL,
		ReleaseNotes:   latest.Body,
	}, nil
}

func autoUpdateIfNeeded(ctx context.Context, repo string) (updateNotice, error) {
	notice, err := checkForUpdate(ctx, repo)
	if err != nil {
		return updateNotice{}, nil
	}
	if notice.Kind != updateNoticeAvailable {
		return notice, nil
	}
	applied, err := applyReleaseUpdate(ctx, repo, notice.LatestVersion)
	if err != nil {
		notice.Kind = updateNoticeFailed
		notice.Error = err.Error()
		return notice, nil
	}
	if applied {
		notice.Kind = updateNoticeUpdated
		return notice, nil
	}
	notice.Kind = updateNoticeFailed
	notice.Error = "update was not applied"
	return notice, nil
}

func applyReleaseUpdate(ctx context.Context, repo, tag string) (bool, error) {
	if !isSelfUpdateSupported() {
		return false, errors.New("self-update is only supported on macOS and Linux")
	}
	release, err := fetchGitHubRelease(ctx, repo, tag)
	if err != nil {
		return false, err
	}
	asset, ok := selectReleaseAsset(release, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return false, fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		exe = mustAbs(exe)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), "codex-session-search-update-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	downloadCtx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()
	if err := downloadReleaseAsset(downloadCtx, asset.BrowserDownloadURL, tmp); err != nil {
		return false, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	if runtime.GOOS == "windows" {
		return false, errors.New("windows self-update is not supported by this build")
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return false, err
	}
	return true, nil
}

func fetchLatestGitHubRelease(ctx context.Context, repo string) (githubRelease, error) {
	return fetchGitHubRelease(ctx, repo, "latest")
}

func fetchGitHubRelease(ctx context.Context, repo, ref string) (githubRelease, error) {
	var release githubRelease
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/%s", repo, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return release, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codex-session-search")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return release, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		text := strings.TrimSpace(string(data))
		if text == "" {
			return release, fmt.Errorf("github api request failed: %s", resp.Status)
		}
		return release, fmt.Errorf("github api request failed: %s: %s", resp.Status, text)
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return release, err
	}
	return release, nil
}

func downloadReleaseAsset(ctx context.Context, url string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "codex-session-search")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		text := strings.TrimSpace(string(data))
		if text == "" {
			return fmt.Errorf("download failed: %s", resp.Status)
		}
		return fmt.Errorf("download failed: %s: %s", resp.Status, text)
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return err
	}
	return nil
}

func selectReleaseAsset(release githubRelease, goos, goarch string) (githubReleaseAsset, bool) {
	want := releaseAssetName(goos, goarch)
	for _, asset := range release.Assets {
		if asset.Name == want {
			return asset, true
		}
	}
	return githubReleaseAsset{}, false
}

func releaseAssetName(goos, goarch string) string {
	name := fmt.Sprintf("codex-session-search_%s_%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func isSelfUpdateSupported() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

func shouldAutoCheckUpdates() bool {
	if isDevVersion(version) {
		return false
	}
	if os.Getenv("CODEX_SESSION_SEARCH_DISABLE_AUTO_UPDATE") != "" {
		return false
	}
	return true
}

func mustAbs(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func compareVersions(current, latest string) int {
	if current == latest {
		return 0
	}
	ci, okCurrent := parseSemanticVersion(current)
	li, okLatest := parseSemanticVersion(latest)
	if okCurrent && okLatest {
		if ci.major != li.major {
			return compareInt(ci.major, li.major)
		}
		if ci.minor != li.minor {
			return compareInt(ci.minor, li.minor)
		}
		if ci.patch != li.patch {
			return compareInt(ci.patch, li.patch)
		}
		if ci.pre == li.pre {
			return 0
		}
		if ci.pre == "" {
			return 1
		}
		if li.pre == "" {
			return -1
		}
		return strings.Compare(ci.pre, li.pre)
	}
	return strings.Compare(current, latest)
}

type semanticVersion struct {
	major int
	minor int
	patch int
	pre   string
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	matches := semverPattern.FindStringSubmatch(normalizedVersion(value))
	if len(matches) != 5 {
		return semanticVersion{}, false
	}
	major, err := parseInt(matches[1])
	if err != nil {
		return semanticVersion{}, false
	}
	minor, err := parseInt(matches[2])
	if err != nil {
		return semanticVersion{}, false
	}
	patch, err := parseInt(matches[3])
	if err != nil {
		return semanticVersion{}, false
	}
	return semanticVersion{
		major: major,
		minor: minor,
		patch: patch,
		pre:   matches[4],
	}, true
}

func parseInt(value string) (int, error) {
	var n int
	_, err := fmt.Sscanf(value, "%d", &n)
	return n, err
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func formatUpdateNotice(notice updateNotice) string {
	switch notice.Kind {
	case updateNoticeUpdated:
		return fmt.Sprintf("Updated to %s", notice.LatestVersion)
	case updateNoticeAvailable:
		return fmt.Sprintf("Update available: %s", notice.LatestVersion)
	case updateNoticeFailed:
		if notice.LatestVersion != "" {
			return fmt.Sprintf("Update available: %s", notice.LatestVersion)
		}
		return "Update failed"
	case updateNoticeCurrent:
		return fmt.Sprintf("Up to date: %s", notice.CurrentVersion)
	default:
		return ""
	}
}

func updateNoticeLines(notice updateNotice, width int, maxLines int) []string {
	if maxLines <= 0 || width <= 0 {
		return nil
	}
	var lines []string
	appendLine := func(line string) {
		if line == "" || len(lines) >= maxLines {
			return
		}
		lines = append(lines, fitUpdateLine(line, width))
	}
	appendLine(formatUpdateNotice(notice))
	if notice.Error != "" {
		appendLine("Auto update: " + notice.Error)
	}
	for _, line := range splitUpdateNotes(notice.ReleaseNotes) {
		if len(lines) >= maxLines {
			break
		}
		appendLine(line)
	}
	if notice.ReleaseURL != "" && len(lines) < maxLines {
		appendLine(notice.ReleaseURL)
	}
	return lines
}

func splitUpdateNotes(notes string) []string {
	notes = strings.ReplaceAll(notes, "\r\n", "\n")
	raw := strings.Split(notes, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "##") {
			continue
		}
		lines = append(lines, strings.TrimLeft(line, "-* "))
	}
	return lines
}

func fitUpdateLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	runes := []rune(line)
	if len(runes) <= width {
		return line
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

func buildReleaseNotes(commitLines []string) string {
	var buf bytes.Buffer
	buf.WriteString("## Changes\n")
	buf.WriteString("\n")
	for _, line := range commitLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		buf.WriteString("- ")
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	return strings.TrimSpace(buf.String())
}

func runUpgradeCommand(args []string) int {
	if len(args) != 0 {
		fmt.Fprintf(os.Stderr, "error: unknown upgrade flag: %s\n", strings.Join(args, " "))
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateDownloadTimeout+updateCheckTimeout)
	defer cancel()
	notice, err := upgradeFromGitHub(ctx, releaseRepository)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	printUpdateNotice(os.Stdout, notice)
	if notice.Kind == updateNoticeFailed {
		return 1
	}
	return 0
}

func upgradeFromGitHub(ctx context.Context, repo string) (updateNotice, error) {
	notice, err := checkForUpdate(ctx, repo)
	if err != nil {
		return updateNotice{}, err
	}
	if notice.Kind == updateNoticeCurrent {
		return notice, nil
	}
	applied, err := applyReleaseUpdate(ctx, repo, notice.LatestVersion)
	if err != nil {
		notice.Kind = updateNoticeFailed
		notice.Error = err.Error()
		return notice, nil
	}
	if !applied {
		notice.Kind = updateNoticeFailed
		notice.Error = "update was not applied"
		return notice, nil
	}
	notice.Kind = updateNoticeUpdated
	if err := saveUpdateState(updateState{
		TagName:    notice.LatestVersion,
		Version:    normalizedVersion(notice.LatestVersion),
		Notes:      notice.ReleaseNotes,
		ReleaseURL: notice.ReleaseURL,
		UpdatedAt:  time.Now().Format(time.RFC3339),
		Shown:      false,
	}); err != nil {
		return notice, err
	}
	return notice, nil
}

func printUpdateNotice(out io.Writer, notice updateNotice) {
	lines := updateNoticeLines(notice, 100, 8)
	if notice.Kind == updateNoticeCurrent {
		fmt.Fprintf(out, "Already up to date: %s\n", nonEmpty(notice.CurrentVersion, version))
		return
	}
	if notice.Kind == updateNoticeFailed {
		fmt.Fprintf(out, "%s\n", formatUpdateNotice(notice))
		if notice.ReleaseURL != "" {
			fmt.Fprintf(out, "%s\n", notice.ReleaseURL)
		}
		for _, line := range lines[1:] {
			fmt.Fprintln(out, line)
		}
		return
	}
	fmt.Fprintf(out, "%s\n", formatUpdateNotice(notice))
	if notice.ReleaseURL != "" {
		fmt.Fprintf(out, "%s\n", notice.ReleaseURL)
	}
	for _, line := range lines[1:] {
		fmt.Fprintln(out, line)
	}
}
