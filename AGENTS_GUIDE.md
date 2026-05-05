# Agent Installation Guide

This guide is for AI coding agents installing `github.com/rkbkosp/codex-session-search` for a user.

`codex-session-search` is a Go CLI/TUI for searching local Codex session history under `~/.codex/sessions`.

## Important Notes For Agents

- This repository is **not** a Codex skill. It does not contain `SKILL.md`.
- Do **not** install it with Codex skill installers.
- Prefer installing the latest GitHub release first.
- Fall back to building from source only when there is no suitable release asset for the user's OS or architecture.
- The current `go.mod` declares `module codex-session-search`, so avoid `go install github.com/rkbkosp/codex-session-search@latest` unless the module path changes in the future.
- Prefer installing the binary to `~/.local/bin/codex-session-search`.
- Do not enable the background daemon unless the user explicitly wants automatic indexing.

## Requirements

Before installing, verify:

```bash
go version
```

The project requires Go 1.22 or newer.

The user should have Codex session data under:

```bash
~/.codex/sessions
```

## Preferred Install: GitHub Releases

Check the latest release and download the matching asset for the user's OS and architecture.

Example with `gh`:

```bash
repo="rkbkosp/codex-session-search"
tmpdir="$(mktemp -d)"
release_tag="$(gh release view --repo "$repo" --json tagName --jq .tagName)"
gh release download --repo "$repo" "$release_tag" --dir "$tmpdir"
```

Release assets are named by platform, for example:

```text
codex-session-search_darwin_arm64
codex-session-search_linux_amd64
codex-session-search_windows_amd64.exe
```

If the release assets include a platform match for the user, install that binary to the user-local bin directory:

```bash
mkdir -p "$HOME/.local/bin"
asset="codex-session-search_darwin_arm64" # replace with the user's OS/architecture asset
install -m 0755 "$tmpdir/$asset" "$HOME/.local/bin/codex-session-search"
```

If the release ships archives, unpack the matching one and install the binary from inside the archive.

After installing a release build, verify the embedded version and optional updater:

```bash
codex-session-search --version
codex-session-search upgrade
```

## Fallback Install: Build From Source

Use this only when the latest release does not provide a suitable OS/architecture asset.

```bash
tmpdir="$(mktemp -d)"
git clone --depth 1 https://github.com/rkbkosp/codex-session-search.git "$tmpdir/codex-session-search"

mkdir -p "$HOME/.local/bin"

cd "$tmpdir/codex-session-search"
go build -trimpath -ldflags="-s -w" -o "$HOME/.local/bin/codex-session-search" .
```

Verify the binary:

```bash
"$HOME/.local/bin/codex-session-search" --help
```

If `~/.local/bin` is already on `PATH`, this should also work:

```bash
codex-session-search --help
```

## PATH Check

Check whether the command is available directly:

```bash
which codex-session-search
```

If it is not found, tell the user to add this to their shell config:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

For zsh, that usually means adding it to:

```bash
~/.zshrc
```

Then reload the shell:

```bash
source ~/.zshrc
```

## Initial Index Refresh

After installation, refresh the index once:

```bash
codex-session-search index refresh
```

Then verify status:

```bash
codex-session-search index status
```

Expected output should show:

```text
Indexed sessions: <number>
Updated at: <timestamp>
```

## Test Search

Run a small one-shot CLI query to confirm search works:

```bash
codex-session-search --limit 1 codex
```

If there are matching sessions, output should include a result and a resume command like:

```text
resume codex resume <session-id>
```

## Optional: Background Daemon

Only install the daemon if the user wants the index to stay fresh automatically.

```bash
codex-session-search daemon install --interval 15s
```

Check daemon status:

```bash
codex-session-search daemon status
```

Stop daemon:

```bash
codex-session-search daemon stop
```

Uninstall daemon:

```bash
codex-session-search daemon uninstall
```

## Common Commands

Open the main TUI:

```bash
codex-session-search
```

Open the search TUI with a prefilled query:

```bash
codex-session-search "query"
```

Search all indexed sessions with one-shot CLI output:

```bash
codex-session-search --limit 10 "query"
```

Search recent sessions:

```bash
codex-session-search --last 3d "query"
```

Search assistant messages only:

```bash
codex-session-search --assistant-only "query"
```

Search user messages only:

```bash
codex-session-search --user-only "query"
```

Show expanded output:

```bash
codex-session-search --view full --limit 5 "query"
```

Emit JSON:

```bash
codex-session-search --json --limit 20 "query"
```

## Troubleshooting

### SKILL.md not found

This means the agent tried to install the repository as a Codex skill. That is incorrect.

Install it as a Go binary instead.

### codex-session-search: command not found

Use the absolute path first:

```bash
"$HOME/.local/bin/codex-session-search" --help
```

Then ensure `~/.local/bin` is on `PATH`.

### No indexed sessions

Run:

```bash
codex-session-search index refresh
```

Then check:

```bash
codex-session-search index status
```

### Search is slow

Confirm the index exists:

```bash
codex-session-search index status
```

If needed, rebuild it:

```bash
codex-session-search index refresh
```

For large histories, ask the user whether they want the daemon installed.

## Recommended Agent Completion Message

After a successful install, summarize:

```text
Installed codex-session-search to ~/.local/bin/codex-session-search.
Verified the CLI with --help.
Refreshed the index and indexed N sessions.
You can open the TUI with: codex-session-search
```
