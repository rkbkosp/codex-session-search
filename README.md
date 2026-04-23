# codex-session-search

Local CLI for searching Codex session history stored under `~/.codex/sessions`.

It supports:

- Full-text search across user and assistant natural-language messages
- Session title matching
- Context snippets around each hit
- Compact default terminal view plus expandable full view
- ANSI highlighting in interactive terminals
- `codex resume <session-id>` output for every match
- Absolute date filters (`--from`, `--to`, `--on`)
- Relative time filters (`--last 3d`, `--last 3h`, `--last 90min`, `--last 3mon`)
- Assistant-only or user-only search
- Persistent lightweight index
- Continuous background refresh on macOS via LaunchAgent or on Linux via `systemd --user`

The tool defaults to `~/.codex` as Codex home and searches `~/.codex/sessions`.

## Why This Exists

Raw Codex session files are append-only JSONL logs. They are accurate, but direct full scans become slow once the session directory grows into hundreds of files or multiple gigabytes.

This project solves that in two layers:

1. Extract a lightweight index containing only searchable natural-language messages and core session metadata
2. Keep that index fresh with a background daemon so normal searches stay fast

On the current machine, raw sessions are about `1.3G`, while the extracted index is about `23M`.

## Requirements

- Go 1.22+
- Codex session data under `~/.codex`
- macOS with `launchctl`, or Linux with `systemd --user`, if you want built-in background daemon management

Search itself is plain Go and does not require third-party dependencies.

## Source Layout

```text
.
├── README.md
├── go.mod
├── main.go
├── index.go
├── daemon.go
└── runtime/                  # generated at runtime, not source
```

File roles:

- `main.go`: CLI parsing, raw search fallback, text/json output
- `index.go`: lightweight index storage, incremental refresh, indexed search
- `daemon.go`: index/daemon subcommands, macOS LaunchAgent and Linux systemd user-service management

## Build

Build in place:

```bash
go build -o codex-session-search .
```

Install to a typical user-local bin directory:

```bash
mkdir -p ~/.local/bin
go build -trimpath -ldflags="-s -w" -o ~/.local/bin/codex-session-search .
```

Optional compatibility alias for the earlier typoed command name:

```bash
ln -sf ~/.local/bin/codex-session-search ~/.local/bin/codex-sesssion-search
```

## Quick Start

Search all indexed sessions:

```bash
codex-session-search "什么是Go语言"
```

Show expanded output:

```bash
codex-session-search --view full --limit 5 "drama_workspace"
```

Search only assistant replies:

```bash
codex-session-search --assistant-only "SQLite"
```

Search within a date range:

```bash
codex-session-search --from 2026-04-01 --to 2026-04-20 "renderwarden"
```

Search within a relative window:

```bash
codex-session-search --last 3d "SRT"
codex-session-search --last 3h --assistant-only "上下文"
codex-session-search --last 90min "drama_workspace"
```

Search one day only:

```bash
codex-session-search --on 2026-04-20 "SRT"
```

Emit JSON:

```bash
codex-session-search --json --last 3h "codex session"
```

## Search Behavior

By default, the tool searches:

- session titles from `session_index.jsonl`
- user natural-language messages
- assistant natural-language messages

It intentionally ignores:

- tool calls
- tool outputs
- reasoning payloads
- developer/system wrapper text

Every result includes:

- `session-id`
- title
- session date
- optional `cwd`
- hit count
- surrounding context snippets
- a ready-to-run resume command

Example:

```text
resume: codex resume 019da989-f055-73c3-a63a-be89183a180b
```

## CLI Usage

### Main Search Command

```bash
codex-session-search [flags] <query>
```

Flags:

- `--from YYYY-MM-DD`: inclusive start date
- `--to YYYY-MM-DD`: inclusive end date
- `--on YYYY-MM-DD`: single day shortcut
- `--last SPAN`: relative window such as `3d`, `3mon`, `3h`, `90min`
- `--limit N`: max number of printed results, `0` means all
- `--snippets N`: max number of context blocks per session
- `--root PATH`: Codex home directory, default `~/.codex`
- `--json`: JSON output
- `--case-sensitive`: case-sensitive matching
- `--role all|assistant|user`: role filter
- `--view compact|full`: terminal output style, default `compact`
- `--assistant-only`: shortcut for `--role assistant`
- `--user-only`: shortcut for `--role user`

Notes:

- `--last` cannot be combined with `--from`, `--to`, or `--on`
- `--limit` only controls output size, not how many candidate sessions are evaluated
- Search prefers the lightweight index; if indexed search cannot be used, the code still retains a raw-scan fallback path
- ANSI color/highlighting is enabled only when writing to an interactive terminal

## Index Commands

Refresh the persistent index manually:

```bash
codex-session-search index refresh
```

Inspect index status:

```bash
codex-session-search index status
```

Use a non-default Codex home:

```bash
codex-session-search index refresh --root /path/to/.codex
```

### What The Index Stores

The index is intentionally much smaller than raw sessions because it stores only:

- session id
- source path
- date
- started timestamp
- title
- updated timestamp
- cwd
- user/assistant natural-language messages

It does not duplicate:

- tool call JSON
- tool output blobs
- encrypted reasoning payloads
- most wrapper metadata

## Background Daemon

The daemon continuously refreshes the lightweight index in the background.

- On macOS it is managed as a LaunchAgent
- On Linux, including Ubuntu, it is managed as a `systemd --user` service

### Install And Start

```bash
codex-session-search daemon install --interval 15s
```

This does three things:

1. Performs an initial index refresh
2. Writes a service definition for the current platform
3. Registers and starts the service with `launchctl` on macOS or `systemctl --user` on Linux

### Status

```bash
codex-session-search daemon status
```

### Stop

```bash
codex-session-search daemon stop
```

### Start Again

```bash
codex-session-search daemon start
```

### Uninstall

```bash
codex-session-search daemon uninstall
```

### Change The Refresh Interval

```bash
codex-session-search daemon install --interval 30s
codex-session-search daemon install --interval 5m
```

The interval is parsed by Go's `time.ParseDuration`, so common values such as `15s`, `30s`, `1m`, `5m` are valid.

## Runtime Files

For a Codex root like `~/.codex`, runtime files are stored under:

```text
~/.local/share/codex-session-search/runtime/<hash>/
```

Typical contents:

```text
runtime/<hash>/
├── state.json
├── daemon-status.json
├── daemon.stdout.log
├── daemon.stderr.log
└── sessions/
    ├── 0166da8720ba8cde.jsonl
    ├── 01b82402978d8b4c.jsonl
    └── ...
```

Meaning:

- `state.json`: source file metadata and per-session index metadata
- `daemon-status.json`: last daemon heartbeat and refresh status
- `daemon.stdout.log`: daemon stdout log
- `daemon.stderr.log`: daemon stderr log
- `sessions/*.jsonl`: extracted lightweight per-session message logs

The service file path depends on platform:

```text
macOS: ~/Library/LaunchAgents/<label>.plist
Linux: ~/.config/systemd/user/<label>.service
```

The `<label>` includes a hash derived from the configured Codex root path, so different roots get isolated runtime directories and service labels.

## Performance Notes

There are two search modes:

1. Indexed search
2. Raw JSONL fallback

Indexed search is the normal path and is dramatically faster on larger histories because it avoids reparsing raw session envelopes, tool payloads, and non-searchable content.

For large histories, background indexing is the recommended mode.

## Troubleshooting

### Search Feels Slow

Check whether the index exists and the daemon is running:

```bash
codex-session-search index status
codex-session-search daemon status
```

If needed, force a rebuild:

```bash
codex-session-search index refresh
```

### Daemon Is Installed But Not Running

Check status:

```bash
codex-session-search daemon status
```

Inspect logs:

```bash
tail -n 100 ~/.local/share/codex-session-search/runtime/<hash>/daemon.stderr.log
tail -n 100 ~/.local/share/codex-session-search/runtime/<hash>/daemon.stdout.log
```

On Linux you can also inspect the user service directly:

```bash
systemctl --user status <label>.service
journalctl --user -u <label>.service -n 100
```

Try reinstalling:

```bash
codex-session-search daemon uninstall
codex-session-search daemon install --interval 15s
```

### `systemctl --user` Cannot Connect To Bus

On Linux, the daemon uses the per-user systemd manager.

If you see an error about failing to connect to the user bus, make sure the account has an active user session, or enable lingering:

```bash
loginctl enable-linger "$USER"
```

### `--last` Does Not Combine With `--from`

This is intentional. Use either:

```bash
codex-session-search --last 3d "query"
```

or:

```bash
codex-session-search --from 2026-04-18 --to 2026-04-21 "query"
```

### Search Returns Only 10 Results

The default output limit is `10`.

Use:

```bash
codex-session-search --limit 50 "query"
codex-session-search --limit 0 "query"
```

## Development Notes

- The daemon integrates with `launchctl` on macOS and `systemctl --user` on Linux
- The index format is designed to be disposable; you can delete the runtime directory and rebuild it
- The raw search path still exists as a fallback implementation
- The project currently uses only the Go standard library

## Typical Workflow

One-time setup:

```bash
go build -trimpath -ldflags="-s -w" -o ~/.local/bin/codex-session-search .
codex-session-search daemon install --interval 15s
```

Daily use:

```bash
codex-session-search "drama_workspace"
codex-session-search --assistant-only --last 3h "SQLite"
codex-session-search --json --limit 20 "SRT"
```
