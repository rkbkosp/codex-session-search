package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	updateCheckTimeout    = 8 * time.Second
	updateDownloadTimeout = 90 * time.Second
	updateVersionTimeout  = 5 * time.Second
	updateFailureCooldown = 6 * time.Hour
	updateStateFileName   = "update-state.json"
	updateChecksumName    = "checksums.txt"
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
	Kind               updateNoticeKind
	CurrentVersion     string
	LatestVersion      string
	ReleaseURL         string
	ReleaseNotes       string
	Error              string
	DaemonRestarted    bool
	DaemonRestartError string
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
	TagName            string `json:"tag_name"`
	Version            string `json:"version"`
	Notes              string `json:"notes,omitempty"`
	ReleaseURL         string `json:"release_url,omitempty"`
	UpdatedAt          string `json:"updated_at,omitempty"`
	Shown              bool   `json:"shown,omitempty"`
	DaemonRestarted    bool   `json:"daemon_restarted,omitempty"`
	DaemonRestartError string `json:"daemon_restart_error,omitempty"`
	FailedTagName      string `json:"failed_tag_name,omitempty"`
	Failure            string `json:"failure,omitempty"`
	FailedAt           string `json:"failed_at,omitempty"`
}

type updateApplyResult struct {
	Applied            bool
	DaemonRestarted    bool
	DaemonRestartError string
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
		Kind:               updateNoticeUpdated,
		CurrentVersion:     currentVersion,
		LatestVersion:      state.TagName,
		ReleaseURL:         state.ReleaseURL,
		ReleaseNotes:       state.Notes,
		DaemonRestarted:    state.DaemonRestarted,
		DaemonRestartError: state.DaemonRestartError,
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
	if skipped, failure := shouldSkipRecentFailedUpdate(notice.LatestVersion); skipped {
		notice.Kind = updateNoticeFailed
		notice.Error = failure
		return notice, nil
	}
	applied, err := applyReleaseUpdate(ctx, repo, notice.LatestVersion)
	if err != nil {
		notice.Kind = updateNoticeFailed
		notice.Error = err.Error()
		_ = saveFailedUpdateState(notice)
		return notice, nil
	}
	if applied.Applied {
		notice.Kind = updateNoticeUpdated
		notice.DaemonRestarted = applied.DaemonRestarted
		notice.DaemonRestartError = applied.DaemonRestartError
		_ = saveAppliedUpdateState(notice)
		return notice, nil
	}
	notice.Kind = updateNoticeFailed
	notice.Error = "update was not applied"
	return notice, nil
}

func applyReleaseUpdate(ctx context.Context, repo, tag string) (updateApplyResult, error) {
	if !isSelfUpdateSupported() {
		return updateApplyResult{}, errors.New("self-update is only supported on macOS and Linux")
	}
	release, err := fetchGitHubRelease(ctx, repo, tag)
	if err != nil {
		return updateApplyResult{}, err
	}
	asset, ok := selectReleaseAsset(release, runtime.GOOS, runtime.GOARCH)
	if !ok {
		return updateApplyResult{}, fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	checksumAsset, ok := selectChecksumAsset(release)
	if !ok {
		return updateApplyResult{}, fmt.Errorf("release asset %s was not found", updateChecksumName)
	}
	checksums, err := downloadReleaseChecksums(ctx, checksumAsset)
	if err != nil {
		return updateApplyResult{}, err
	}
	expectedChecksum, err := checksumForAsset(checksums, asset.Name)
	if err != nil {
		return updateApplyResult{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return updateApplyResult{}, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		exe = mustAbs(exe)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), "codex-session-search-update-*")
	if err != nil {
		return updateApplyResult{}, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	downloadCtx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()
	if err := downloadReleaseAsset(downloadCtx, asset.BrowserDownloadURL, tmp); err != nil {
		return updateApplyResult{}, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return updateApplyResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return updateApplyResult{}, err
	}
	if err := verifyDownloadedChecksum(tmpPath, asset.Name, expectedChecksum); err != nil {
		return updateApplyResult{}, err
	}
	if err := verifyReleaseBinary(ctx, tmpPath, tag); err != nil {
		return updateApplyResult{}, err
	}

	if runtime.GOOS == "windows" {
		return updateApplyResult{}, errors.New("windows self-update is not supported by this build")
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return updateApplyResult{}, err
	}
	result := updateApplyResult{Applied: true}
	restarted, restartErr := restartRunningDaemon()
	result.DaemonRestarted = restarted
	if restartErr != nil {
		result.DaemonRestartError = restartErr.Error()
	}
	return result, nil
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

func selectChecksumAsset(release githubRelease) (githubReleaseAsset, bool) {
	for _, asset := range release.Assets {
		if asset.Name == updateChecksumName {
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

func shouldSkipRecentFailedUpdate(tag string) (bool, string) {
	state, err := loadUpdateState()
	if err != nil {
		return false, ""
	}
	if normalizedVersion(state.FailedTagName) != normalizedVersion(tag) || state.Failure == "" || state.FailedAt == "" {
		return false, ""
	}
	failedAt, err := time.Parse(time.RFC3339, state.FailedAt)
	if err != nil {
		return false, ""
	}
	if time.Since(failedAt) > updateFailureCooldown {
		return false, ""
	}
	return true, state.Failure
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

func downloadReleaseChecksums(ctx context.Context, asset githubReleaseAsset) ([]byte, error) {
	var buf bytes.Buffer
	if err := downloadReleaseAsset(ctx, asset.BrowserDownloadURL, &buf); err != nil {
		return nil, fmt.Errorf("download %s: %w", updateChecksumName, err)
	}
	return buf.Bytes(), nil
}

func checksumForAsset(checksums []byte, assetName string) (string, error) {
	for _, rawLine := range strings.Split(string(checksums), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[1], "*")
		if name != assetName {
			continue
		}
		if len(sum) != sha256.Size*2 {
			return "", fmt.Errorf("invalid checksum length for %s", assetName)
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", fmt.Errorf("invalid checksum for %s: %w", assetName, err)
		}
		return sum, nil
	}
	return "", fmt.Errorf("checksum for %s was not found in %s", assetName, updateChecksumName)
}

func verifyDownloadedChecksum(path, assetName, expected string) error {
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if actual != strings.ToLower(expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, expected, actual)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, handle); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyReleaseBinary(ctx context.Context, path, tag string) error {
	probeCtx, cancel := context.WithTimeout(ctx, updateVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, path, "--version")
	output, err := cmd.CombinedOutput()
	if probeCtx.Err() != nil {
		return fmt.Errorf("version check timed out for downloaded binary")
	}
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return fmt.Errorf("version check failed for downloaded binary: %w", err)
		}
		return fmt.Errorf("version check failed for downloaded binary: %w: %s", err, normalizeWhitespace(text))
	}
	if err := validateVersionOutput(text, tag); err != nil {
		return err
	}
	return nil
}

func validateVersionOutput(output, tag string) error {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) < 2 || fields[0] != "codex-session-search" {
		return fmt.Errorf("downloaded binary returned unexpected version output: %s", nonEmpty(output, "(empty)"))
	}
	got := normalizedVersion(fields[1])
	want := normalizedVersion(tag)
	if got == "" || got != want {
		return fmt.Errorf("downloaded binary version %s did not match %s", fields[1], tag)
	}
	return nil
}

func restartRunningDaemon() (bool, error) {
	root, err := expandPath(defaultRoot)
	if err != nil {
		return false, err
	}
	manager, err := newIndexManager(root)
	if err != nil {
		return false, err
	}
	switch runtime.GOOS {
	case "darwin":
		if !fileExists(manager.LaunchAgentPath) {
			return false, nil
		}
		loaded, _ := isLaunchdDaemonLoaded(manager)
		if !loaded {
			return false, nil
		}
		if err := kickstartLaunchdDaemon(manager); err != nil {
			return false, err
		}
		return true, nil
	case "linux":
		if !fileExists(manager.SystemdUnitPath) {
			return false, nil
		}
		status, err := readSystemdUnitStatus(manager)
		if err != nil {
			return false, err
		}
		if status.ActiveState != "active" {
			return false, nil
		}
		if err := runSystemctlUser("restart", systemdUnitName(manager)); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
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
	if notice.DaemonRestartError != "" {
		appendLine("Daemon restart: " + notice.DaemonRestartError)
	} else if notice.DaemonRestarted {
		appendLine("Daemon restarted.")
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
		_ = saveFailedUpdateState(notice)
		return notice, nil
	}
	if !applied.Applied {
		notice.Kind = updateNoticeFailed
		notice.Error = "update was not applied"
		return notice, nil
	}
	notice.Kind = updateNoticeUpdated
	notice.DaemonRestarted = applied.DaemonRestarted
	notice.DaemonRestartError = applied.DaemonRestartError
	if err := saveAppliedUpdateState(notice); err != nil {
		return notice, err
	}
	return notice, nil
}

func saveAppliedUpdateState(notice updateNotice) error {
	return saveUpdateState(updateState{
		TagName:            notice.LatestVersion,
		Version:            normalizedVersion(notice.LatestVersion),
		Notes:              notice.ReleaseNotes,
		ReleaseURL:         notice.ReleaseURL,
		UpdatedAt:          time.Now().Format(time.RFC3339),
		Shown:              false,
		DaemonRestarted:    notice.DaemonRestarted,
		DaemonRestartError: notice.DaemonRestartError,
	})
}

func saveFailedUpdateState(notice updateNotice) error {
	state, _ := loadUpdateState()
	state.FailedTagName = notice.LatestVersion
	state.Failure = notice.Error
	state.FailedAt = time.Now().Format(time.RFC3339)
	return saveUpdateState(state)
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
