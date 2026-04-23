package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultDaemonInterval = 15 * time.Second

type daemonCommandConfig struct {
	Root     string
	Interval time.Duration
}

type daemonStatus struct {
	Label                  string `json:"label"`
	Root                   string `json:"root"`
	Pid                    int    `json:"pid"`
	Interval               string `json:"interval"`
	StartedAt              string `json:"started_at,omitempty"`
	LastRefreshStartedAt   string `json:"last_refresh_started_at,omitempty"`
	LastRefreshCompletedAt string `json:"last_refresh_completed_at,omitempty"`
	LastError              string `json:"last_error,omitempty"`
	IndexedSessions        int    `json:"indexed_sessions,omitempty"`
	ChangedSessions        int    `json:"changed_sessions,omitempty"`
	DeletedSessions        int    `json:"deleted_sessions,omitempty"`
	Running                bool   `json:"running"`
}

type systemdUnitStatus struct {
	LoadState     string
	ActiveState   string
	UnitFileState string
}

func handleSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "index":
		return true, runIndexCommand(args[1:])
	case "daemon":
		return true, runDaemonCommand(args[1:])
	default:
		return false, 0
	}
}

func runIndexCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: missing index subcommand")
		return 2
	}
	cfg, err := parseDaemonCommandConfig(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	manager, err := newIndexManager(cfg.Root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	switch args[0] {
	case "refresh":
		result, err := refreshIndex(manager)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("Index refreshed.\n")
		fmt.Printf("Indexed sessions: %d\n", result.IndexedSessions)
		fmt.Printf("Changed sessions: %d\n", result.ChangedSessions)
		fmt.Printf("Deleted sessions: %d\n", result.DeletedSessions)
		fmt.Printf("Unchanged sessions: %d\n", result.UnchangedSessions)
		fmt.Printf("Updated at: %s\n", result.UpdatedAt.Format(time.RFC3339))
		return 0
	case "status":
		state, err := loadIndexState(manager)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Printf("Index root: %s\n", manager.Root)
		fmt.Printf("Index storage: %s\n", manager.StorageDir)
		fmt.Printf("Indexed sessions: %d\n", len(state.Sessions))
		fmt.Printf("Updated at: %s\n", nonEmpty(state.UpdatedAt, "(never)"))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "error: unknown index subcommand: %s\n", args[0])
		return 2
	}
}

func runDaemonCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "error: missing daemon subcommand")
		return 2
	}
	cfg, err := parseDaemonCommandConfig(args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	manager, err := newIndexManager(cfg.Root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	switch args[0] {
	case "run":
		return runDaemonLoop(manager, cfg.Interval)
	case "install":
		return installDaemon(manager, cfg.Interval)
	case "start":
		return startDaemon(manager)
	case "stop":
		return stopDaemon(manager)
	case "uninstall":
		return uninstallDaemon(manager)
	case "status":
		return printDaemonStatus(manager)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown daemon subcommand: %s\n", args[0])
		return 2
	}
}

func parseDaemonCommandConfig(args []string) (daemonCommandConfig, error) {
	root, err := expandPath(defaultRoot)
	if err != nil {
		return daemonCommandConfig{}, err
	}
	cfg := daemonCommandConfig{
		Root:     root,
		Interval: defaultDaemonInterval,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--root":
			value, next, err := nextValue(args, i, args[i])
			if err != nil {
				return cfg, err
			}
			root, err := expandPath(value)
			if err != nil {
				return cfg, err
			}
			cfg.Root = root
			i = next
		case "--interval":
			value, next, err := nextValue(args, i, args[i])
			if err != nil {
				return cfg, err
			}
			interval, err := time.ParseDuration(value)
			if err != nil {
				return cfg, fmt.Errorf("invalid --interval: %w", err)
			}
			if interval <= 0 {
				return cfg, errors.New("--interval must be greater than 0")
			}
			cfg.Interval = interval
			i = next
		default:
			return cfg, fmt.Errorf("unknown flag: %s", args[i])
		}
	}
	return cfg, nil
}

func runDaemonLoop(manager indexManager, interval time.Duration) int {
	if err := ensureIndexDirs(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	status := daemonStatus{
		Label:     manager.Label,
		Root:      manager.Root,
		Pid:       os.Getpid(),
		Interval:  interval.String(),
		StartedAt: time.Now().Format(time.RFC3339),
		Running:   true,
	}
	_ = writeJSONFileAtomic(manager.StatusPath, status)

	refreshOnce := func() {
		status.Running = true
		status.Pid = os.Getpid()
		status.LastRefreshStartedAt = time.Now().Format(time.RFC3339)
		_ = writeJSONFileAtomic(manager.StatusPath, status)

		result, err := refreshIndex(manager)
		if err != nil {
			status.LastError = err.Error()
		} else {
			status.LastError = ""
			status.IndexedSessions = result.IndexedSessions
			status.ChangedSessions = result.ChangedSessions
			status.DeletedSessions = result.DeletedSessions
			status.LastRefreshCompletedAt = result.UpdatedAt.Format(time.RFC3339)
		}
		_ = writeJSONFileAtomic(manager.StatusPath, status)
	}

	refreshOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			status.Running = false
			status.Pid = 0
			_ = writeJSONFileAtomic(manager.StatusPath, status)
			return 0
		case <-ticker.C:
			refreshOnce()
		}
	}
}

func installDaemon(manager indexManager, interval time.Duration) int {
	if err := ensureDaemonPlatformSupported(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := ensureIndexDirs(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	initial, err := refreshIndex(manager)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: initial index refresh failed: %v\n", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: locate executable: %v\n", err)
		return 1
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchdDaemon(manager, exe, interval, initial)
	case "linux":
		return installSystemdDaemon(manager, exe, interval, initial)
	default:
		fmt.Fprintf(os.Stderr, "error: daemon management is not supported on %s\n", runtime.GOOS)
		return 1
	}
}

func startDaemon(manager indexManager) int {
	if err := ensureDaemonPlatformSupported(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	switch runtime.GOOS {
	case "darwin":
		if !fileExists(manager.LaunchAgentPath) {
			fmt.Fprintf(os.Stderr, "error: launch agent not installed: %s\n", manager.LaunchAgentPath)
			return 1
		}
		if err := bootstrapLaunchdDaemon(manager); err != nil && !isAlreadyBootstrapped(err) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		if err := kickstartLaunchdDaemon(manager); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	case "linux":
		if !fileExists(manager.SystemdUnitPath) {
			fmt.Fprintf(os.Stderr, "error: systemd unit not installed: %s\n", manager.SystemdUnitPath)
			return 1
		}
		if err := runSystemctlUser("daemon-reload"); err != nil {
			fmt.Fprintf(os.Stderr, "error: reload systemd user manager: %v\n", err)
			return 1
		}
		if err := runSystemctlUser("start", systemdUnitName(manager)); err != nil {
			fmt.Fprintf(os.Stderr, "error: start systemd unit: %v\n", err)
			return 1
		}
	}

	fmt.Printf("Daemon started: %s\n", daemonServiceName(manager))
	return 0
}

func stopDaemon(manager indexManager) int {
	if err := ensureDaemonPlatformSupported(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	switch runtime.GOOS {
	case "darwin":
		if err := bootoutLaunchdDaemon(manager); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
	case "linux":
		if err := runSystemctlUser("stop", systemdUnitName(manager)); err != nil && !isIgnorableSystemdUnitError(err) {
			fmt.Fprintf(os.Stderr, "error: stop systemd unit: %v\n", err)
			return 1
		}
	}

	fmt.Printf("Daemon stopped: %s\n", daemonServiceName(manager))
	return 0
}

func uninstallDaemon(manager indexManager) int {
	if err := ensureDaemonPlatformSupported(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	switch runtime.GOOS {
	case "darwin":
		_ = bootoutLaunchdDaemon(manager)
		if fileExists(manager.LaunchAgentPath) {
			if err := os.Remove(manager.LaunchAgentPath); err != nil {
				fmt.Fprintf(os.Stderr, "error: remove launch agent: %v\n", err)
				return 1
			}
		}
	case "linux":
		if fileExists(manager.SystemdUnitPath) {
			if err := runSystemctlUser("disable", "--now", systemdUnitName(manager)); err != nil && !isIgnorableSystemdUnitError(err) {
				fmt.Fprintf(os.Stderr, "error: disable systemd unit: %v\n", err)
				return 1
			}
			if err := os.Remove(manager.SystemdUnitPath); err != nil {
				fmt.Fprintf(os.Stderr, "error: remove systemd unit: %v\n", err)
				return 1
			}
			if err := runSystemctlUser("daemon-reload"); err != nil {
				fmt.Fprintf(os.Stderr, "error: reload systemd user manager: %v\n", err)
				return 1
			}
		}
		_ = runSystemctlUser("reset-failed", systemdUnitName(manager))
	}

	fmt.Printf("Daemon uninstalled: %s\n", daemonServiceName(manager))
	return 0
}

func printDaemonStatus(manager indexManager) int {
	status := daemonStatus{}
	if fileExists(manager.StatusPath) {
		data, err := os.ReadFile(manager.StatusPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: read daemon status: %v\n", err)
			return 1
		}
		if err := json.Unmarshal(data, &status); err != nil {
			fmt.Fprintf(os.Stderr, "error: parse daemon status: %v\n", err)
			return 1
		}
	}
	fmt.Printf("Label: %s\n", manager.Label)
	fmt.Printf("Root: %s\n", manager.Root)

	switch runtime.GOOS {
	case "darwin":
		loaded, loadError := isLaunchdDaemonLoaded(manager)
		fmt.Printf("Manager: launchd\n")
		fmt.Printf("LaunchAgent: %s\n", manager.LaunchAgentPath)
		fmt.Printf("Installed: %t\n", fileExists(manager.LaunchAgentPath))
		fmt.Printf("Loaded: %t\n", loaded)
		fmt.Printf("Running: %t\n", status.Running && loaded)
		if loadError != "" {
			fmt.Printf("Launchctl: %s\n", loadError)
		}
	case "linux":
		unitStatus, err := readSystemdUnitStatus(manager)
		loaded := unitStatus.LoadState == "loaded"
		running := unitStatus.ActiveState == "active"

		fmt.Printf("Manager: systemd-user\n")
		fmt.Printf("Systemd unit: %s\n", manager.SystemdUnitPath)
		fmt.Printf("Installed: %t\n", fileExists(manager.SystemdUnitPath))
		fmt.Printf("Loaded: %t\n", loaded)
		fmt.Printf("Running: %t\n", status.Running && running)
		if unitStatus.UnitFileState != "" {
			fmt.Printf("Unit file state: %s\n", unitStatus.UnitFileState)
		}
		if unitStatus.ActiveState != "" {
			fmt.Printf("Active state: %s\n", unitStatus.ActiveState)
		}
		if err != nil {
			fmt.Printf("Systemd: %s\n", err)
		}
	default:
		fmt.Printf("Manager: unsupported (%s)\n", runtime.GOOS)
		fmt.Printf("Installed: false\n")
		fmt.Printf("Loaded: false\n")
		fmt.Printf("Running: %t\n", status.Running && processRunning(status.Pid))
	}
	if status.Pid != 0 {
		fmt.Printf("PID: %d\n", status.Pid)
	}
	if status.Interval != "" {
		fmt.Printf("Interval: %s\n", status.Interval)
	}
	if status.StartedAt != "" {
		fmt.Printf("Started at: %s\n", status.StartedAt)
	}
	if status.LastRefreshCompletedAt != "" {
		fmt.Printf("Last refresh: %s\n", status.LastRefreshCompletedAt)
	}
	if status.IndexedSessions > 0 {
		fmt.Printf("Indexed sessions: %d\n", status.IndexedSessions)
	}
	if status.LastError != "" {
		fmt.Printf("Last error: %s\n", status.LastError)
	}
	return 0
}

func installLaunchdDaemon(manager indexManager, executable string, interval time.Duration, initial refreshResult) int {
	plist := buildLaunchAgentPlist(manager, executable, interval)
	if err := writeFileAtomic(manager.LaunchAgentPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write launch agent: %v\n", err)
		return 1
	}

	_ = bootoutLaunchdDaemon(manager)
	if err := bootstrapLaunchdDaemon(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: load launch agent: %v\n", err)
		return 1
	}
	if err := kickstartLaunchdDaemon(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: start daemon: %v\n", err)
		return 1
	}

	fmt.Printf("Daemon installed.\n")
	fmt.Printf("Manager: launchd\n")
	fmt.Printf("Label: %s\n", manager.Label)
	fmt.Printf("LaunchAgent: %s\n", manager.LaunchAgentPath)
	fmt.Printf("Index storage: %s\n", manager.StorageDir)
	fmt.Printf("Initial indexed sessions: %d\n", initial.IndexedSessions)
	fmt.Printf("Refresh interval: %s\n", interval)
	return 0
}

func installSystemdDaemon(manager indexManager, executable string, interval time.Duration, initial refreshResult) int {
	unit := buildSystemdUnit(manager, executable, interval)
	if err := writeFileAtomic(manager.SystemdUnitPath, []byte(unit), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write systemd unit: %v\n", err)
		return 1
	}
	if err := runSystemctlUser("daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "error: reload systemd user manager: %v\n", err)
		return 1
	}
	if err := runSystemctlUser("enable", systemdUnitName(manager)); err != nil {
		fmt.Fprintf(os.Stderr, "error: enable systemd unit: %v\n", err)
		return 1
	}
	if err := runSystemctlUser("restart", systemdUnitName(manager)); err != nil {
		if err := runSystemctlUser("start", systemdUnitName(manager)); err != nil {
			fmt.Fprintf(os.Stderr, "error: start systemd unit: %v\n", err)
			return 1
		}
	}

	fmt.Printf("Daemon installed.\n")
	fmt.Printf("Manager: systemd-user\n")
	fmt.Printf("Label: %s\n", manager.Label)
	fmt.Printf("Systemd unit: %s\n", manager.SystemdUnitPath)
	fmt.Printf("Index storage: %s\n", manager.StorageDir)
	fmt.Printf("Initial indexed sessions: %d\n", initial.IndexedSessions)
	fmt.Printf("Refresh interval: %s\n", interval)
	return 0
}

func buildLaunchAgentPlist(manager indexManager, executable string, interval time.Duration) string {
	args := []string{
		executable,
		"daemon",
		"run",
		"--root",
		manager.Root,
		"--interval",
		interval.String(),
	}

	var argLines []string
	for _, arg := range args {
		argLines = append(argLines, fmt.Sprintf("    <string>%s</string>", xmlEscape(arg)))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>WorkingDirectory</key>
  <string>%s</string>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, xmlEscape(manager.Label), strings.Join(argLines, "\n"), xmlEscape(manager.StorageDir), xmlEscape(manager.StdoutLogPath), xmlEscape(manager.StderrLogPath))
}

func buildSystemdUnit(manager indexManager, executable string, interval time.Duration) string {
	args := []string{
		executable,
		"daemon",
		"run",
		"--root",
		manager.Root,
		"--interval",
		interval.String(),
	}

	var quotedArgs []string
	for _, arg := range args {
		quotedArgs = append(quotedArgs, systemdQuoteArg(arg))
	}

	return fmt.Sprintf(`[Unit]
Description=%s

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s
Restart=always
RestartSec=1s
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, systemdEscapeUnitValue("codex-session-search background index refresh for "+manager.Root), systemdEscapeUnitValue(manager.StorageDir), strings.Join(quotedArgs, " "), systemdEscapeUnitValue(manager.StdoutLogPath), systemdEscapeUnitValue(manager.StderrLogPath))
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func daemonConfigPath(manager indexManager) string {
	switch runtime.GOOS {
	case "darwin":
		return manager.LaunchAgentPath
	case "linux":
		return manager.SystemdUnitPath
	default:
		return ""
	}
}

func daemonServiceName(manager indexManager) string {
	switch runtime.GOOS {
	case "linux":
		return systemdUnitName(manager)
	default:
		return manager.Label
	}
}

func ensureDaemonPlatformSupported() error {
	switch runtime.GOOS {
	case "darwin", "linux":
		return nil
	default:
		return fmt.Errorf("daemon management is not supported on %s", runtime.GOOS)
	}
}

func launchctlDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func launchdServiceTarget(manager indexManager) string {
	return launchctlDomain() + "/" + manager.Label
}

func bootstrapLaunchdDaemon(manager indexManager) error {
	return runLaunchctl("bootstrap", launchctlDomain(), manager.LaunchAgentPath)
}

func kickstartLaunchdDaemon(manager indexManager) error {
	return runLaunchctl("kickstart", "-k", launchdServiceTarget(manager))
}

func bootoutLaunchdDaemon(manager indexManager) error {
	target := manager.LaunchAgentPath
	if !fileExists(target) {
		target = launchdServiceTarget(manager)
	}
	err := runLaunchctl("bootout", launchctlDomain(), target)
	if err != nil && (strings.Contains(err.Error(), "No such process") || strings.Contains(err.Error(), "Could not find service")) {
		return nil
	}
	return err
}

func isLaunchdDaemonLoaded(manager indexManager) (bool, string) {
	output, err := runLaunchctlWithOutput("print", launchdServiceTarget(manager))
	if err != nil {
		return false, strings.TrimSpace(output)
	}
	return true, strings.TrimSpace(output)
}

func isAlreadyBootstrapped(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "service already loaded") || strings.Contains(err.Error(), "already bootstrapped")
}

func runLaunchctl(args ...string) error {
	output, err := runLaunchctlWithOutput(args...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	return nil
}

func runLaunchctlWithOutput(args ...string) (string, error) {
	cmd := exec.Command("launchctl", args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func systemdUnitName(manager indexManager) string {
	return manager.Label + ".service"
}

func readSystemdUnitStatus(manager indexManager) (systemdUnitStatus, error) {
	output, err := runSystemctlUserWithOutput(
		"show",
		systemdUnitName(manager),
		"--property=LoadState",
		"--property=ActiveState",
		"--property=UnitFileState",
	)
	if err != nil {
		return systemdUnitStatus{}, wrapSystemctlError(err, output)
	}

	status := systemdUnitStatus{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			status.LoadState = value
		case "ActiveState":
			status.ActiveState = value
		case "UnitFileState":
			status.UnitFileState = value
		}
	}
	return status, nil
}

func runSystemctlUser(args ...string) error {
	output, err := runSystemctlUserWithOutput(args...)
	if err != nil {
		return wrapSystemctlError(err, output)
	}
	return nil
}

func runSystemctlUserWithOutput(args ...string) (string, error) {
	cmdArgs := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", cmdArgs...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func wrapSystemctlError(err error, output string) error {
	message := strings.TrimSpace(output)
	if message == "" {
		message = err.Error()
	}
	if strings.Contains(message, "Failed to connect to user scope bus") || strings.Contains(message, "Failed to connect to bus") {
		message += " (hint: ensure the current account has an active systemd user session, or enable lingering with `loginctl enable-linger $USER`)"
	}
	return fmt.Errorf("%w: %s", err, message)
}

func isIgnorableSystemdUnitError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not loaded") ||
		strings.Contains(text, "not-found") ||
		strings.Contains(text, "no such file")
}

func systemdQuoteArg(value string) string {
	return strconv.Quote(systemdEscapeUnitValue(value))
}

func systemdEscapeUnitValue(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
