package main

import (
	"strings"
	"testing"
)

func TestTUIRouting(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantTUI    bool
		wantSubcmd bool
	}{
		{name: "no args", wantTUI: true},
		{name: "plain query", args: []string{"drama", "workspace"}, wantTUI: true},
		{name: "flagged query stays cli", args: []string{"--limit", "1", "drama"}},
		{name: "index subcommand stays cli", args: []string{"index", "refresh"}, wantSubcmd: true},
		{name: "daemon subcommand stays cli", args: []string{"daemon", "status"}, wantSubcmd: true},
		{name: "single index can be a search term", args: []string{"index"}, wantTUI: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldHandleSubcommand(tt.args); got != tt.wantSubcmd {
				t.Fatalf("shouldHandleSubcommand() = %t, want %t", got, tt.wantSubcmd)
			}
			if got := shouldRunTUI(tt.args); got != tt.wantTUI {
				t.Fatalf("shouldRunTUI() = %t, want %t", got, tt.wantTUI)
			}
		})
	}
}

func TestNormalizeTUICommitQuery(t *testing.T) {
	got, err := normalizeTUIQuery(" FB5EF21 ", tuiSearchCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got != "fb5ef21" {
		t.Fatalf("query = %q, want fb5ef21", got)
	}
	if _, err := normalizeTUIQuery("not-a-hash", tuiSearchCommit); err == nil {
		t.Fatal("expected invalid commit query error")
	}
}

func TestLaunchCommandString(t *testing.T) {
	id := "019df31b-37dd-7b42-b161-cabd94aaaed7"
	if got := launchCommandString(id, tuiLaunchCLI); got != "codex resume "+id {
		t.Fatalf("cli command = %q", got)
	}
	if got := launchCommandString(id, tuiLaunchApp); !strings.Contains(got, "codex://threads/"+id) {
		t.Fatalf("app command = %q, want deep link", got)
	}
}

func TestTUIResultListOmitsDuplicateIDAndHits(t *testing.T) {
	model := newTUIModel("/tmp/codex", "SRT")
	model.activeTerm = "SRT"
	model.results = []result{{
		ID:         "019df31b-37dd-7b42-b161-cabd94aaaed7",
		Title:      "SRT cleanup",
		Date:       "2026-05-04",
		CWD:        "/repo/drama_workspace",
		MatchCount: 30,
	}}

	view := model.renderResultList()
	if strings.Contains(view, "019df31b-37dd...") {
		t.Fatalf("result list contains truncated duplicate id: %q", view)
	}
	if strings.Contains(view, "hits:") {
		t.Fatalf("result list contains hit count: %q", view)
	}
	if !strings.Contains(view, "019df31b-37dd-7b42-b161-cabd94aaaed7") {
		t.Fatalf("result list omitted full id: %q", view)
	}
}

func TestTUIPreviewOmitsSessionFilePath(t *testing.T) {
	model := newTUIModel("/tmp/codex", "SRT")
	model.activeTerm = "SRT"
	model.results = []result{{
		ID:    "019df31b-37dd-7b42-b161-cabd94aaaed7",
		Title: "SRT cleanup",
		Path:  "/Users/huangwei/.codex/sessions/2026/05/04/session.jsonl",
		Snippets: []snippet{{
			Match: message{Role: "user", Text: "fix SRT"},
		}},
	}}

	view := model.renderPreview()
	if strings.Contains(view, "file:") {
		t.Fatalf("preview contains session file path: %q", view)
	}
}

func TestTUIViewportScrollsToBottomWithinBorderedPane(t *testing.T) {
	content := strings.Join([]string{
		"line-01",
		"line-02",
		"line-03",
		"line-04",
		"line-05",
		"line-06",
		"line-07",
		"line-08",
		"line-09",
		"line-10",
	}, "\n")

	view := renderTUIViewport(content, 24, 4, 8, tuiPaneStyle)
	if !strings.Contains(view, "line-10") {
		t.Fatalf("viewport did not show bottom line after scroll: %q", view)
	}
	if strings.Contains(view, "line-07") {
		t.Fatalf("viewport was clamped by outer height instead of inner pane height: %q", view)
	}
}
