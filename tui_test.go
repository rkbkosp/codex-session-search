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
