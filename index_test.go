package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractIndexedSessionCommitRefs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-05-04T10-00-00-019df31b-37dd-7b42-b161-cabd94aaaed7.jsonl")
	lines := []string{
		jsonLine(t, eventEnvelope{
			Timestamp: "2026-05-04T02:00:00Z",
			Type:      "session_meta",
			Payload: mustRawJSON(t, sessionMeta{
				ID:        "019df31b-37dd-7b42-b161-cabd94aaaed7",
				Timestamp: "2026-05-04T02:00:00Z",
				CWD:       "/repo",
			}),
		}),
		`{"timestamp":"2026-05-04T02:01:00Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git rev-parse --short HEAD\",\"workdir\":\"/repo\",\"yield_time_ms\":1000}","call_id":"call_short"}}`,
		`{"timestamp":"2026-05-04T02:01:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_short","output":"Command: /bin/zsh -lc 'git rev-parse --short HEAD'\nChunk ID: abcdef\nOutput:\nfb5ef21\n"}}`,
		`{"timestamp":"2026-05-04T02:01:59Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"rtk git rev-parse --short HEAD\",\"workdir\":\"/repo\",\"yield_time_ms\":1000}","call_id":"call_rtk"}}`,
		`{"timestamp":"2026-05-04T02:02:00Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_rtk","command":["/bin/zsh","-lc","rtk git rev-parse --short HEAD"],"cwd":"/repo","aggregated_output":"7ee251f\n","exit_code":0}}`,
		`{"timestamp":"2026-05-04T02:02:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_rtk","output":"Chunk ID: cc526c\nOutput:\n7ee251f\n"}}`,
	}
	if err := os.WriteFile(path, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, _, commits, err := extractIndexedSession(sessionFile{
		Path: path,
		Date: "2026-05-04",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.CommitCount != 2 {
		t.Fatalf("CommitCount = %d, want 2", meta.CommitCount)
	}
	if len(commits) != 2 {
		t.Fatalf("len(commits) = %d, want 2", len(commits))
	}
	if commits[0].Hash != "fb5ef21" || commits[0].Source != "function_call_output" {
		t.Fatalf("first commit = %#v", commits[0])
	}
	if commits[1].Hash != "7ee251f" || commits[1].Source != "exec_command_end" {
		t.Fatalf("second commit = %#v", commits[1])
	}
}

func TestCommitMatchHasQueryHandlesPrefixes(t *testing.T) {
	match := commitMatch{Hash: "fb5ef21"}
	if !commitMatchHasQuery(match, "fb5") {
		t.Fatal("short query prefix did not match indexed hash")
	}
	if !commitMatchHasQuery(match, "fb5ef21abcd") {
		t.Fatal("longer query prefix did not match indexed short hash")
	}
	if commitMatchHasQuery(match, "abc1234") {
		t.Fatal("unrelated hash matched")
	}
}

func TestSearchCommitsWithIndex(t *testing.T) {
	temp := t.TempDir()
	root := filepath.Join(temp, "codex")
	sessionDir := filepath.Join(root, "sessions", "2026", "05", "04")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-2026-05-04T10-00-00-019df31b-37dd-7b42-b161-cabd94aaaed7.jsonl")
	lines := []string{
		jsonLine(t, eventEnvelope{
			Timestamp: "2026-05-04T02:00:00Z",
			Type:      "session_meta",
			Payload: mustRawJSON(t, sessionMeta{
				ID:        "019df31b-37dd-7b42-b161-cabd94aaaed7",
				Timestamp: "2026-05-04T02:00:00Z",
				CWD:       "/repo",
			}),
		}),
		`{"timestamp":"2026-05-04T02:01:00Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"git rev-parse --short HEAD\",\"workdir\":\"/repo\"}","call_id":"call_short"}}`,
		`{"timestamp":"2026-05-04T02:01:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_short","output":"Output:\nfb5ef21\n"}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(joinLines(lines)), 0o644); err != nil {
		t.Fatal(err)
	}

	storage := filepath.Join(temp, "runtime")
	manager := indexManager{
		Root:        root,
		StorageDir:  storage,
		SessionsDir: filepath.Join(storage, "sessions"),
		CommitsDir:  filepath.Join(storage, "commits"),
		StatePath:   filepath.Join(storage, "state.json"),
	}
	if _, err := refreshIndex(manager); err != nil {
		t.Fatal(err)
	}

	results, warnings, scanned, err := searchCommitsWithIndex(manager, config{
		Query:       "fb5ef21",
		CommitQuery: "fb5ef21",
		Role:        "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v", warnings)
	}
	if scanned != 1 {
		t.Fatalf("scanned = %d, want 1", scanned)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if len(results[0].CommitMatches) != 1 || results[0].CommitMatches[0].Hash != "fb5ef21" {
		t.Fatalf("commit matches = %#v", results[0].CommitMatches)
	}
}

func jsonLine(t *testing.T, value interface{}) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustRawJSON(t *testing.T, value interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func joinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}
