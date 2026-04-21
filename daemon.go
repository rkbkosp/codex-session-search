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
	plist := buildLaunchAgentPlist(manager, exe, interval)
	if err := writeFileAtomic(manager.LaunchAgentPath, []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write launch agent: %v\n", err)
		return 1
	}

	_ = bootoutDaemon(manager)
	if err := bootstrapDaemon(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: load launch agent: %v\n", err)
		return 1
	}
	if err := kickstartDaemon(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: start daemon: %v\n", err)
		return 1
	}

	fmt.Printf("Daemon installed.\n")
	fmt.Printf("Label: %s\n", manager.Label)
	fmt.Printf("LaunchAgent: %s\n", manager.LaunchAgentPath)
	fmt.Printf("Index storage: %s\n", manager.StorageDir)
	fmt.Printf("Initial indexed sessions: %d\n", initial.IndexedSessions)
	fmt.Printf("Refresh interval: %s\n", interval)
	return 0
}

func startDaemon(manager indexManager) int {
	if !fileExists(manager.LaunchAgentPath) {
		fmt.Fprintf(os.Stderr, "error: launch agent not installed: %s\n", manager.LaunchAgentPath)
		return 1
	}
	if err := bootstrapDaemon(manager); err != nil && !isAlreadyBootstrapped(err) {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	if err := kickstartDaemon(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("Daemon started: %s\n", manager.Label)
	return 0
}

func stopDaemon(manager indexManager) int {
	if err := bootoutDaemon(manager); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Printf("Daemon stopped: %s\n", manager.Label)
	return 0
}

func uninstallDaemon(manager indexManager) int {
	_ = bootoutDaemon(manager)
	if fileExists(manager.LaunchAgentPath) {
		if err := os.Remove(manager.LaunchAgentPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: remove launch agent: %v\n", err)
			return 1
		}
	}
	fmt.Printf("Daemon uninstalled: %s\n", manager.Label)
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
	loaded, loadError := isDaemonLoaded(manager)

	fmt.Printf("Label: %s\n", manager.Label)
	fmt.Printf("Root: %s\n", manager.Root)
	fmt.Printf("LaunchAgent: %s\n", manager.LaunchAgentPath)
	fmt.Printf("Installed: %t\n", fileExists(manager.LaunchAgentPath))
	fmt.Printf("Loaded: %t\n", loaded)
	fmt.Printf("Running: %t\n", status.Running && loaded)
	if loadError != "" {
		fmt.Printf("Launchctl: %s\n", loadError)
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

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func launchctlDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func serviceTarget(manager indexManager) string {
	return launchctlDomain() + "/" + manager.Label
}

func bootstrapDaemon(manager indexManager) error {
	return runLaunchctl("bootstrap", launchctlDomain(), manager.LaunchAgentPath)
}

func kickstartDaemon(manager indexManager) error {
	return runLaunchctl("kickstart", "-k", serviceTarget(manager))
}

func bootoutDaemon(manager indexManager) error {
	target := manager.LaunchAgentPath
	if !fileExists(target) {
		target = serviceTarget(manager)
	}
	err := runLaunchctl("bootout", launchctlDomain(), target)
	if err != nil && (strings.Contains(err.Error(), "No such process") || strings.Contains(err.Error(), "Could not find service")) {
		return nil
	}
	return err
}

func isDaemonLoaded(manager indexManager) (bool, string) {
	output, err := runLaunchctlWithOutput("print", serviceTarget(manager))
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
