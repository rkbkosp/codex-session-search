package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const indexVersion = 1

type indexManager struct {
	Root            string
	StorageDir      string
	SessionsDir     string
	StatePath       string
	StatusPath      string
	StdoutLogPath   string
	StderrLogPath   string
	LaunchAgentPath string
	Label           string
}

type indexState struct {
	Version   int                           `json:"version"`
	Root      string                        `json:"root"`
	UpdatedAt string                        `json:"updated_at,omitempty"`
	Sessions  map[string]indexedSessionMeta `json:"sessions"`
}

type indexedSessionMeta struct {
	SourcePath      string `json:"source_path"`
	SessionID       string `json:"session_id"`
	Date            string `json:"date,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	Title           string `json:"title,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
	CWD             string `json:"cwd,omitempty"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	IndexFile       string `json:"index_file"`
	MessageCount    int    `json:"message_count"`
}

type refreshResult struct {
	IndexedSessions   int       `json:"indexed_sessions"`
	ChangedSessions   int       `json:"changed_sessions"`
	DeletedSessions   int       `json:"deleted_sessions"`
	UnchangedSessions int       `json:"unchanged_sessions"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func newIndexManager(root string) (indexManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return indexManager{}, err
	}
	hash := shortHash(root)
	storageDir := filepath.Join(home, ".local", "share", "codex-session-search", "runtime", hash)
	label := "com.huangwei.codex-session-search." + hash
	return indexManager{
		Root:            root,
		StorageDir:      storageDir,
		SessionsDir:     filepath.Join(storageDir, "sessions"),
		StatePath:       filepath.Join(storageDir, "state.json"),
		StatusPath:      filepath.Join(storageDir, "daemon-status.json"),
		StdoutLogPath:   filepath.Join(storageDir, "daemon.stdout.log"),
		StderrLogPath:   filepath.Join(storageDir, "daemon.stderr.log"),
		LaunchAgentPath: filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		Label:           label,
	}, nil
}

func ensureIndexDirs(manager indexManager) error {
	if err := os.MkdirAll(manager.SessionsDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(manager.LaunchAgentPath), 0o755); err != nil {
		return err
	}
	return nil
}

func loadIndexState(manager indexManager) (indexState, error) {
	data, err := os.ReadFile(manager.StatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return indexState{
				Version:  indexVersion,
				Root:     manager.Root,
				Sessions: make(map[string]indexedSessionMeta),
			}, nil
		}
		return indexState{}, err
	}
	var state indexState
	if err := json.Unmarshal(data, &state); err != nil {
		return indexState{}, err
	}
	if state.Sessions == nil {
		state.Sessions = make(map[string]indexedSessionMeta)
	}
	if state.Version == 0 {
		state.Version = indexVersion
	}
	if state.Root == "" {
		state.Root = manager.Root
	}
	return state, nil
}

func saveIndexState(manager indexManager, state indexState) error {
	state.Version = indexVersion
	state.Root = manager.Root
	return writeJSONFileAtomic(manager.StatePath, state)
}

func refreshIndex(manager indexManager) (refreshResult, error) {
	if err := ensureIndexDirs(manager); err != nil {
		return refreshResult{}, err
	}

	state, err := loadIndexState(manager)
	if err != nil {
		return refreshResult{}, err
	}

	threadIndex, err := loadSessionIndex(filepath.Join(manager.Root, "session_index.jsonl"))
	if err != nil {
		return refreshResult{}, err
	}

	files, err := collectSessionFiles(filepath.Join(manager.Root, "sessions"), "", "", time.Time{}, time.Time{})
	if err != nil {
		return refreshResult{}, err
	}

	current := make(map[string]sessionFile, len(files))
	result := refreshResult{}
	for _, file := range files {
		current[file.Path] = file

		info, err := os.Stat(file.Path)
		if err != nil {
			return refreshResult{}, err
		}

		prev, ok := state.Sessions[file.Path]
		if ok &&
			prev.Size == info.Size() &&
			prev.ModTimeUnixNano == info.ModTime().UnixNano() &&
			prev.IndexFile != "" &&
			fileExists(filepath.Join(manager.StorageDir, prev.IndexFile)) {
			result.UnchangedSessions++
			continue
		}

		meta, messages, err := extractIndexedSession(file, threadIndex)
		if err != nil {
			return refreshResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}

		meta.Size = info.Size()
		meta.ModTimeUnixNano = info.ModTime().UnixNano()
		meta.IndexFile = filepath.Join("sessions", indexFileName(file.Path))

		indexPath := filepath.Join(manager.StorageDir, meta.IndexFile)
		if err := writeIndexedMessages(indexPath, messages); err != nil {
			return refreshResult{}, err
		}

		state.Sessions[file.Path] = meta
		result.ChangedSessions++
	}

	for sourcePath, meta := range state.Sessions {
		if _, ok := current[sourcePath]; ok {
			continue
		}
		if meta.IndexFile != "" {
			_ = os.Remove(filepath.Join(manager.StorageDir, meta.IndexFile))
		}
		delete(state.Sessions, sourcePath)
		result.DeletedSessions++
	}

	result.IndexedSessions = len(state.Sessions)
	result.UpdatedAt = time.Now()
	state.UpdatedAt = result.UpdatedAt.Format(time.RFC3339)
	if err := saveIndexState(manager, state); err != nil {
		return refreshResult{}, err
	}
	return result, nil
}

func extractIndexedSession(file sessionFile, threadIndex map[string]indexEntry) (indexedSessionMeta, []message, error) {
	id := extractSessionID(file.Path)
	meta := indexedSessionMeta{
		SourcePath: file.Path,
		SessionID:  id,
		Date:       file.Date,
	}
	if entry, ok := threadIndex[id]; ok {
		meta.Title = normalizeWhitespace(entry.ThreadName)
		meta.UpdatedAt = entry.UpdatedAt
	}
	if meta.StartedAt == "" && !file.StartedAt.IsZero() {
		meta.StartedAt = file.StartedAt.Format(time.RFC3339)
	}

	handle, err := os.Open(file.Path)
	if err != nil {
		return indexedSessionMeta{}, nil, err
	}
	defer handle.Close()

	var messages []message
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var env eventEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		switch env.Type {
		case "session_meta":
			var raw sessionMeta
			if err := json.Unmarshal(env.Payload, &raw); err != nil {
				continue
			}
			if raw.ID != "" {
				meta.SessionID = raw.ID
				if entry, ok := threadIndex[raw.ID]; ok {
					if meta.Title == "" {
						meta.Title = normalizeWhitespace(entry.ThreadName)
					}
					if meta.UpdatedAt == "" {
						meta.UpdatedAt = entry.UpdatedAt
					}
				}
			}
			if raw.CWD != "" {
				meta.CWD = raw.CWD
			}
			if raw.Timestamp != "" {
				meta.StartedAt = raw.Timestamp
			}
		case "response_item":
			msg := extractMessage(env.Timestamp, env.Payload)
			if msg == nil || !searchableRole(msg.Role, "all") {
				continue
			}
			messages = append(messages, *msg)
		}
	}
	if err := scanner.Err(); err != nil {
		return indexedSessionMeta{}, nil, err
	}

	meta.MessageCount = len(messages)
	if meta.Title == "" && len(messages) > 0 {
		meta.Title = fallbackTitle([]string{messages[0].Text})
	}
	return meta, messages, nil
}

func writeIndexedMessages(path string, messages []message) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

func searchWithIndex(manager indexManager, cfg config) ([]result, []string, int, error) {
	state, err := loadIndexState(manager)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(state.Sessions) == 0 {
		if _, err := refreshIndex(manager); err != nil {
			return nil, nil, 0, err
		}
		state, err = loadIndexState(manager)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	candidates := filterIndexedSessions(state, cfg)
	results := make([]result, 0, len(candidates))
	var warnings []string
	for _, meta := range candidates {
		res, err := searchIndexedSession(manager, meta, cfg)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", meta.SourcePath, err))
			continue
		}
		if res != nil {
			results = append(results, *res)
		}
	}
	return results, warnings, len(candidates), nil
}

func filterIndexedSessions(state indexState, cfg config) []indexedSessionMeta {
	from, to := effectiveDateRange(cfg)
	candidates := make([]indexedSessionMeta, 0, len(state.Sessions))
	for _, meta := range state.Sessions {
		if !dateWithinRange(meta.Date, from, to) {
			continue
		}
		if !timeWithinRange(parseIndexedTime(meta), cfg.LastSince, cfg.LastUntil) {
			continue
		}
		candidates = append(candidates, meta)
	}
	sort.Slice(candidates, func(i, j int) bool {
		ti := parseIndexedTime(candidates[i])
		tj := parseIndexedTime(candidates[j])
		if !ti.IsZero() && !tj.IsZero() && !ti.Equal(tj) {
			return ti.After(tj)
		}
		if candidates[i].Date != candidates[j].Date {
			return candidates[i].Date > candidates[j].Date
		}
		return candidates[i].SessionID > candidates[j].SessionID
	})
	return candidates
}

func searchIndexedSession(manager indexManager, meta indexedSessionMeta, cfg config) (*result, error) {
	res := &result{
		ID:        meta.SessionID,
		Title:     meta.Title,
		UpdatedAt: meta.UpdatedAt,
		Date:      meta.Date,
		StartedAt: meta.StartedAt,
		CWD:       meta.CWD,
		Path:      meta.SourcePath,
		Resume:    "codex resume " + meta.SessionID,
	}
	matches := makeMatcher(cfg.Query, cfg.CaseSensitive)
	if res.Title != "" && matches(res.Title) {
		res.TitleMatched = true
	}

	indexPath := filepath.Join(manager.StorageDir, meta.IndexFile)
	handle, err := os.Open(indexPath)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)

	var firstByRole *message
	var prev *message
	var pending []pendingSnippet
	for scanner.Scan() {
		var msg message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if !searchableRole(msg.Role, cfg.Role) {
			continue
		}
		if firstByRole == nil {
			firstByRole = cloneMessage(&msg)
		}
		if len(pending) > 0 {
			after := cloneMessage(&msg)
			for _, item := range pending {
				res.Snippets = append(res.Snippets, snippet{
					Before: item.Before,
					Match:  item.Match,
					After:  after,
				})
			}
			pending = pending[:0]
		}
		if matches(msg.Text) {
			res.MatchCount++
			if cfg.Snippets > 0 && len(res.Snippets)+len(pending) < cfg.Snippets {
				pending = append(pending, pendingSnippet{
					Before: cloneMessage(prev),
					Match:  msg,
				})
			}
		}
		prev = cloneMessage(&msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for _, item := range pending {
		res.Snippets = append(res.Snippets, snippet{
			Before: item.Before,
			Match:  item.Match,
		})
	}

	if res.MatchCount == 0 && res.TitleMatched && len(res.Snippets) == 0 && firstByRole != nil {
		res.Snippets = append(res.Snippets, snippet{Match: *firstByRole})
	}
	if res.MatchCount == 0 && !res.TitleMatched {
		return nil, nil
	}
	return res, nil
}

func parseIndexedTime(meta indexedSessionMeta) time.Time {
	if meta.StartedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, meta.StartedAt); err == nil {
			return parsed
		}
	}
	return extractStartTimeFromFilename(meta.SourcePath)
}

func shortHash(value string) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(filepath.Clean(value)))
	return fmt.Sprintf("%016x", hasher.Sum64())
}

func indexFileName(sourcePath string) string {
	return shortHash(sourcePath) + ".jsonl"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeJSONFileAtomic(path string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
