package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const indexVersion = 3
const commitResolveTimeout = 2 * time.Second
const commitLookupFileName = "commit_lookup.jsonl"

var (
	wholeCommitHashPattern   = regexp.MustCompile(`(?i)^[0-9a-f]{4,40}$`)
	leadingCommitHashPattern = regexp.MustCompile(`(?i)^([0-9a-f]{4,40})(?:\s|$)`)
	commitHashTokenPattern   = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b`)
)

type indexManager struct {
	Root             string
	StorageDir       string
	SessionsDir      string
	CommitsDir       string
	CommitLookupPath string
	StatePath        string
	StatusPath       string
	StdoutLogPath    string
	StderrLogPath    string
	LaunchAgentPath  string
	SystemdUnitPath  string
	Label            string
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
	CommitIndexFile string `json:"commit_index_file,omitempty"`
	MessageCount    int    `json:"message_count"`
	CommitCount     int    `json:"commit_count,omitempty"`
	FullCommitCount int    `json:"full_commit_count,omitempty"`
	CommitResolved  bool   `json:"commit_resolved,omitempty"`
}

type refreshResult struct {
	IndexedSessions   int       `json:"indexed_sessions"`
	IndexedCommitRefs int       `json:"indexed_commit_refs"`
	FullCommitRefs    int       `json:"full_commit_refs"`
	ChangedSessions   int       `json:"changed_sessions"`
	DeletedSessions   int       `json:"deleted_sessions"`
	UnchangedSessions int       `json:"unchanged_sessions"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type refreshOptions struct {
	ResolveCommits bool
	Progress       func(refreshProgress)
}

type refreshProgress struct {
	Stage   string
	Current int
	Total   int
}

const (
	refreshStageLoadState       = "load_state"
	refreshStageLoadSessionInfo = "load_session_info"
	refreshStageCollectSessions = "collect_sessions"
	refreshStageScanSessions    = "scan_sessions"
	refreshStagePruneDeleted    = "prune_deleted"
	refreshStageWriteLookup     = "write_lookup"
	refreshStageSaveState       = "save_state"
	refreshStageComplete        = "complete"
)

type indexedCommitRef struct {
	SessionID  string `json:"session_id"`
	SourcePath string `json:"source_path"`
	Hash       string `json:"hash"`
	FullHash   string `json:"full_hash,omitempty"`
	Timestamp  string `json:"timestamp,omitempty"`
	CWD        string `json:"cwd,omitempty"`
	Command    string `json:"command,omitempty"`
	Source     string `json:"source,omitempty"`
	CallID     string `json:"call_id,omitempty"`
}

func (ref indexedCommitRef) commitMatch() commitMatch {
	return commitMatch{
		Hash:      ref.Hash,
		FullHash:  ref.FullHash,
		Timestamp: ref.Timestamp,
		CWD:       ref.CWD,
		Command:   ref.Command,
		Source:    ref.Source,
		CallID:    ref.CallID,
	}
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
		Root:             root,
		StorageDir:       storageDir,
		SessionsDir:      filepath.Join(storageDir, "sessions"),
		CommitsDir:       filepath.Join(storageDir, "commits"),
		CommitLookupPath: filepath.Join(storageDir, commitLookupFileName),
		StatePath:        filepath.Join(storageDir, "state.json"),
		StatusPath:       filepath.Join(storageDir, "daemon-status.json"),
		StdoutLogPath:    filepath.Join(storageDir, "daemon.stdout.log"),
		StderrLogPath:    filepath.Join(storageDir, "daemon.stderr.log"),
		LaunchAgentPath:  filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		SystemdUnitPath:  filepath.Join(home, ".config", "systemd", "user", label+".service"),
		Label:            label,
	}, nil
}

func ensureIndexDirs(manager indexManager) error {
	if err := os.MkdirAll(manager.SessionsDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(manager.CommitsDir, 0o755); err != nil {
		return err
	}
	if path := daemonConfigPath(manager); path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
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
	return refreshIndexWithOptions(manager, refreshOptions{ResolveCommits: true})
}

func refreshIndexWithOptions(manager indexManager, options refreshOptions) (refreshResult, error) {
	if err := ensureIndexDirs(manager); err != nil {
		return refreshResult{}, err
	}

	emitRefreshProgress(options, refreshProgress{Stage: refreshStageLoadState})
	state, err := loadIndexState(manager)
	if err != nil {
		return refreshResult{}, err
	}

	emitRefreshProgress(options, refreshProgress{Stage: refreshStageLoadSessionInfo})
	threadIndex, err := loadSessionIndex(filepath.Join(manager.Root, "session_index.jsonl"))
	if err != nil {
		return refreshResult{}, err
	}

	emitRefreshProgress(options, refreshProgress{Stage: refreshStageCollectSessions})
	files, err := collectSessionFiles(filepath.Join(manager.Root, "sessions"), "", "", time.Time{}, time.Time{})
	if err != nil {
		return refreshResult{}, err
	}

	current := make(map[string]sessionFile, len(files))
	result := refreshResult{}
	emitRefreshProgress(options, refreshProgress{Stage: refreshStageScanSessions, Current: 0, Total: len(files)})
	for i, file := range files {
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
			prev.CommitIndexFile != "" &&
			(!options.ResolveCommits || prev.CommitResolved) &&
			fileExists(filepath.Join(manager.StorageDir, prev.CommitIndexFile)) &&
			fileExists(filepath.Join(manager.StorageDir, prev.IndexFile)) {
			result.UnchangedSessions++
			if shouldEmitRefreshScanProgress(i+1, len(files)) {
				emitRefreshProgress(options, refreshProgress{Stage: refreshStageScanSessions, Current: i + 1, Total: len(files)})
			}
			continue
		}

		meta, messages, commitRefs, err := extractIndexedSession(file, threadIndex)
		if err != nil {
			return refreshResult{}, fmt.Errorf("%s: %w", file.Path, err)
		}

		meta.Size = info.Size()
		meta.ModTimeUnixNano = info.ModTime().UnixNano()
		meta.IndexFile = filepath.Join("sessions", indexFileName(file.Path))
		meta.CommitIndexFile = filepath.Join("commits", indexFileName(file.Path))
		if options.ResolveCommits {
			commitRefs = resolveCommitFullHashes(commitRefs)
			meta.CommitResolved = true
		}
		meta.CommitCount = len(commitRefs)
		meta.FullCommitCount = countFullCommitMatches(commitRefs)

		indexPath := filepath.Join(manager.StorageDir, meta.IndexFile)
		if err := writeIndexedMessages(indexPath, messages); err != nil {
			return refreshResult{}, err
		}
		commitIndexPath := filepath.Join(manager.StorageDir, meta.CommitIndexFile)
		if err := writeIndexedCommits(commitIndexPath, commitRefs); err != nil {
			return refreshResult{}, err
		}

		state.Sessions[file.Path] = meta
		result.ChangedSessions++
		if shouldEmitRefreshScanProgress(i+1, len(files)) {
			emitRefreshProgress(options, refreshProgress{Stage: refreshStageScanSessions, Current: i + 1, Total: len(files)})
		}
	}

	emitRefreshProgress(options, refreshProgress{Stage: refreshStagePruneDeleted})
	for sourcePath, meta := range state.Sessions {
		if _, ok := current[sourcePath]; ok {
			continue
		}
		if meta.IndexFile != "" {
			_ = os.Remove(filepath.Join(manager.StorageDir, meta.IndexFile))
		}
		if meta.CommitIndexFile != "" {
			_ = os.Remove(filepath.Join(manager.StorageDir, meta.CommitIndexFile))
		}
		delete(state.Sessions, sourcePath)
		result.DeletedSessions++
	}

	result.IndexedSessions = len(state.Sessions)
	result.IndexedCommitRefs = countIndexedCommitRefs(state)
	result.FullCommitRefs = countFullCommitRefs(state)
	result.UpdatedAt = time.Now()
	state.UpdatedAt = result.UpdatedAt.Format(time.RFC3339)
	emitRefreshProgress(options, refreshProgress{Stage: refreshStageWriteLookup})
	if err := writeCommitLookup(manager, state); err != nil {
		return refreshResult{}, err
	}
	emitRefreshProgress(options, refreshProgress{Stage: refreshStageSaveState})
	if err := saveIndexState(manager, state); err != nil {
		return refreshResult{}, err
	}
	emitRefreshProgress(options, refreshProgress{Stage: refreshStageComplete, Current: result.IndexedSessions, Total: result.IndexedSessions})
	return result, nil
}

func emitRefreshProgress(options refreshOptions, progress refreshProgress) {
	if options.Progress != nil {
		options.Progress(progress)
	}
}

func shouldEmitRefreshScanProgress(current, total int) bool {
	return total == 0 || current == 1 || current == total || current%50 == 0
}

func extractIndexedSession(file sessionFile, threadIndex map[string]indexEntry) (indexedSessionMeta, []message, []commitMatch, error) {
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
		return indexedSessionMeta{}, nil, nil, err
	}
	defer handle.Close()

	var messages []message
	var commitRefs []commitMatch
	pendingCommitCalls := make(map[string]commitCommandContext)
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
			if msg != nil && searchableRole(msg.Role, "all") {
				messages = append(messages, *msg)
			}
			if ctx, ok := extractCommitCommandContext(env.Timestamp, env.Payload); ok {
				pendingCommitCalls[ctx.CallID] = ctx
				continue
			}
			if refs := extractCommitRefsFromFunctionCallOutput(env.Timestamp, env.Payload, pendingCommitCalls); len(refs) > 0 {
				commitRefs = append(commitRefs, refs...)
			}
		case "event_msg":
			if refs := extractCommitRefsFromExecCommandEnd(env.Timestamp, env.Payload); len(refs) > 0 {
				commitRefs = append(commitRefs, refs...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return indexedSessionMeta{}, nil, nil, err
	}

	meta.MessageCount = len(messages)
	commitRefs = append(commitRefs, extractCommitRefsFromAssistantMessages(messages, meta.CWD)...)
	commitRefs = dedupeCommitMatches(commitRefs)
	meta.CommitCount = len(commitRefs)
	if meta.Title == "" && len(messages) > 0 {
		meta.Title = fallbackTitle([]string{messages[0].Text})
	}
	return meta, messages, commitRefs, nil
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

func writeIndexedCommits(path string, matches []commitMatch) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, match := range matches {
		if err := enc.Encode(match); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, buf.Bytes(), 0o644)
}

func writeCommitLookup(manager indexManager, state indexState) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	metas := sortedIndexedSessionMetas(state)
	for _, meta := range metas {
		matches, err := loadIndexedCommits(manager, meta)
		if err != nil {
			return err
		}
		for _, match := range matches {
			ref := indexedCommitRef{
				SessionID:  meta.SessionID,
				SourcePath: meta.SourcePath,
				Hash:       match.Hash,
				FullHash:   match.FullHash,
				Timestamp:  match.Timestamp,
				CWD:        match.CWD,
				Command:    match.Command,
				Source:     match.Source,
				CallID:     match.CallID,
			}
			if err := enc.Encode(ref); err != nil {
				return err
			}
		}
	}
	return writeFileAtomic(commitLookupPath(manager), buf.Bytes(), 0o644)
}

func loadCommitLookup(manager indexManager) ([]indexedCommitRef, error) {
	handle, err := os.Open(commitLookupPath(manager))
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	var refs []indexedCommitRef
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var ref indexedCommitRef
		if err := json.Unmarshal(scanner.Bytes(), &ref); err != nil {
			continue
		}
		if ref.SourcePath == "" || ref.Hash == "" {
			continue
		}
		refs = append(refs, ref)
	}
	return refs, scanner.Err()
}

func loadIndexedCommits(manager indexManager, meta indexedSessionMeta) ([]commitMatch, error) {
	if meta.CommitIndexFile == "" {
		return nil, nil
	}
	handle, err := os.Open(filepath.Join(manager.StorageDir, meta.CommitIndexFile))
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	var matches []commitMatch
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var match commitMatch
		if err := json.Unmarshal(scanner.Bytes(), &match); err != nil {
			continue
		}
		if match.Hash == "" {
			continue
		}
		matches = append(matches, match)
	}
	return matches, scanner.Err()
}

func sortedIndexedSessionMetas(state indexState) []indexedSessionMeta {
	metas := make([]indexedSessionMeta, 0, len(state.Sessions))
	for _, meta := range state.Sessions {
		metas = append(metas, meta)
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].SourcePath < metas[j].SourcePath
	})
	return metas
}

func searchWithIndex(manager indexManager, cfg config) ([]result, []string, int, error) {
	return searchWithIndexWithRefreshOptions(manager, cfg, refreshOptions{ResolveCommits: true})
}

func searchWithIndexWithRefreshOptions(manager indexManager, cfg config, options refreshOptions) ([]result, []string, int, error) {
	state, err := loadIndexState(manager)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(state.Sessions) == 0 {
		if _, err := refreshIndexWithOptions(manager, options); err != nil {
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

func searchCommitsWithIndex(manager indexManager, cfg config) ([]result, []string, int, error) {
	return searchCommitsWithIndexWithRefreshOptions(manager, cfg, refreshOptions{ResolveCommits: true})
}

func searchCommitsWithIndexWithRefreshOptions(manager indexManager, cfg config, options refreshOptions) ([]result, []string, int, error) {
	state, err := loadIndexState(manager)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(state.Sessions) == 0 || !commitLookupReady(manager, state) {
		if _, err := refreshIndexWithOptions(manager, options); err != nil {
			return nil, nil, 0, err
		}
		state, err = loadIndexState(manager)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	candidates := filterIndexedSessions(state, cfg)
	candidateByPath := make(map[string]indexedSessionMeta, len(candidates))
	for _, meta := range candidates {
		candidateByPath[meta.SourcePath] = meta
	}

	refs, err := loadCommitLookup(manager)
	if err != nil {
		return nil, nil, 0, err
	}
	resultIndexByPath := make(map[string]int)
	results := make([]result, 0)
	for _, ref := range refs {
		meta, ok := candidateByPath[ref.SourcePath]
		if !ok {
			continue
		}
		match := ref.commitMatch()
		if !commitMatchHasQuery(match, cfg.CommitQuery) {
			continue
		}
		idx, ok := resultIndexByPath[ref.SourcePath]
		if !ok {
			results = append(results, result{
				ID:            meta.SessionID,
				Title:         meta.Title,
				UpdatedAt:     meta.UpdatedAt,
				Date:          meta.Date,
				StartedAt:     meta.StartedAt,
				CWD:           meta.CWD,
				Path:          meta.SourcePath,
				Resume:        "codex resume " + meta.SessionID,
				CommitMatched: true,
			})
			idx = len(results) - 1
			resultIndexByPath[ref.SourcePath] = idx
		}
		results[idx].MatchCount++
		results[idx].CommitMatches = append(results[idx].CommitMatches, match)
	}
	return results, nil, len(candidates), nil
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

func searchIndexedSessionCommits(manager indexManager, meta indexedSessionMeta, cfg config) (*result, error) {
	if meta.CommitIndexFile == "" {
		return nil, nil
	}
	indexPath := filepath.Join(manager.StorageDir, meta.CommitIndexFile)
	handle, err := os.Open(indexPath)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

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

	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var match commitMatch
		if err := json.Unmarshal(scanner.Bytes(), &match); err != nil {
			continue
		}
		if !commitMatchHasQuery(match, cfg.CommitQuery) {
			continue
		}
		res.CommitMatched = true
		res.MatchCount++
		res.CommitMatches = append(res.CommitMatches, match)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !res.CommitMatched {
		return nil, nil
	}
	return res, nil
}

type commitCommandContext struct {
	CallID    string
	Timestamp string
	Command   string
	CWD       string
}

type toolFunctionCall struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	CallID    string          `json:"call_id"`
}

type toolFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type execCommandArguments struct {
	Cmd     string `json:"cmd"`
	Workdir string `json:"workdir"`
}

type execCommandEndPayload struct {
	Type             string   `json:"type"`
	CallID           string   `json:"call_id"`
	Command          []string `json:"command"`
	CWD              string   `json:"cwd"`
	AggregatedOutput string   `json:"aggregated_output"`
	Stdout           string   `json:"stdout"`
	Stderr           string   `json:"stderr"`
	ExitCode         *int     `json:"exit_code"`
}

func extractCommitCommandContext(timestamp string, payload json.RawMessage) (commitCommandContext, bool) {
	var item toolFunctionCall
	if err := json.Unmarshal(payload, &item); err != nil {
		return commitCommandContext{}, false
	}
	if item.Type != "function_call" || item.Name != "exec_command" || item.CallID == "" {
		return commitCommandContext{}, false
	}
	args, err := parseExecCommandArguments(item.Arguments)
	if err != nil || !isGitCommitHashCommand(args.Cmd) {
		return commitCommandContext{}, false
	}
	return commitCommandContext{
		CallID:    item.CallID,
		Timestamp: timestamp,
		Command:   args.Cmd,
		CWD:       args.Workdir,
	}, true
}

func extractCommitRefsFromFunctionCallOutput(timestamp string, payload json.RawMessage, pending map[string]commitCommandContext) []commitMatch {
	var item toolFunctionCallOutput
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil
	}
	if item.Type != "function_call_output" || item.CallID == "" {
		return nil
	}
	ctx, ok := pending[item.CallID]
	if !ok {
		return nil
	}
	delete(pending, item.CallID)
	if ctx.Timestamp == "" {
		ctx.Timestamp = timestamp
	}
	return buildCommitMatches(ctx, item.Output, "function_call_output")
}

func extractCommitRefsFromExecCommandEnd(timestamp string, payload json.RawMessage) []commitMatch {
	var item execCommandEndPayload
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil
	}
	if item.Type != "exec_command_end" {
		return nil
	}
	if item.ExitCode != nil && *item.ExitCode != 0 {
		return nil
	}
	command := shellCommandString(item.Command)
	if !isGitCommitHashCommand(command) {
		return nil
	}
	output := item.AggregatedOutput
	if output == "" {
		output = strings.TrimSpace(item.Stdout + "\n" + item.Stderr)
	}
	ctx := commitCommandContext{
		CallID:    item.CallID,
		Timestamp: timestamp,
		Command:   command,
		CWD:       item.CWD,
	}
	return buildCommitMatches(ctx, output, "exec_command_end")
}

func parseExecCommandArguments(raw json.RawMessage) (execCommandArguments, error) {
	var args execCommandArguments
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		return args, json.Unmarshal([]byte(encoded), &args)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return execCommandArguments{}, err
	}
	return args, nil
}

func isGitCommitHashCommand(command string) bool {
	normalized := strings.ToLower(normalizeWhitespace(command))
	if normalized == "" {
		return false
	}
	if !containsGitCommand(normalized) {
		return false
	}
	if strings.Contains(normalized, "rev-parse") && strings.Contains(normalized, "head") {
		return true
	}
	if strings.Contains(normalized, " log ") && strings.Contains(normalized, "-1") && strings.Contains(normalized, "--oneline") {
		return true
	}
	return false
}

func containsGitCommand(command string) bool {
	fields := splitCommandFields(command)
	for i, field := range fields {
		field = strings.Trim(field, "'\"")
		if filepath.Base(field) == "git" {
			return true
		}
		if filepath.Base(field) == "rtk" && i+1 < len(fields) && filepath.Base(strings.Trim(fields[i+1], "'\"")) == "git" {
			return true
		}
	}
	return false
}

func shellCommandString(command []string) string {
	if len(command) >= 3 {
		shell := filepath.Base(command[0])
		if (shell == "zsh" || shell == "bash" || shell == "sh") && command[1] == "-lc" {
			return command[2]
		}
	}
	return strings.Join(command, " ")
}

func buildCommitMatches(ctx commitCommandContext, output, source string) []commitMatch {
	hashes := extractCommitHashesFromOutput(output)
	matches := make([]commitMatch, 0, len(hashes))
	for _, hash := range hashes {
		match := commitMatch{
			Hash:      hash,
			Timestamp: ctx.Timestamp,
			CWD:       ctx.CWD,
			Command:   ctx.Command,
			Source:    source,
			CallID:    ctx.CallID,
		}
		if len(hash) == 40 {
			match.FullHash = hash
		}
		matches = append(matches, match)
	}
	return matches
}

func extractCommitRefsFromAssistantMessages(messages []message, cwd string) []commitMatch {
	var matches []commitMatch
	for _, msg := range messages {
		if msg.Role != "assistant" || !assistantTextMayContainCommitHash(msg.Text) {
			continue
		}
		for _, hash := range extractCommitHashTokens(msg.Text) {
			match := commitMatch{
				Hash:      hash,
				Timestamp: msg.Timestamp,
				CWD:       cwd,
				Source:    "assistant_message",
			}
			if len(hash) == 40 {
				match.FullHash = hash
			}
			matches = append(matches, match)
		}
	}
	return matches
}

func assistantTextMayContainCommitHash(text string) bool {
	if wholeCommitHashPattern.MatchString(strings.TrimSpace(text)) {
		return true
	}
	lower := strings.ToLower(text)
	markers := []string{
		"commit",
		"git",
		"hash",
		"head",
		"rev-parse",
		"提交",
	}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func extractCommitHashTokens(text string) []string {
	var hashes []string
	seen := make(map[string]bool)
	for _, pair := range commitHashTokenPattern.FindAllStringIndex(text, -1) {
		if commitTokenTouchesHyphen(text, pair[0], pair[1]) {
			continue
		}
		hash := strings.ToLower(text[pair[0]:pair[1]])
		if seen[hash] {
			continue
		}
		seen[hash] = true
		hashes = append(hashes, hash)
	}
	return hashes
}

func commitTokenTouchesHyphen(text string, start, end int) bool {
	return (start > 0 && text[start-1] == '-') || (end < len(text) && text[end] == '-')
}

func resolveCommitFullHashes(matches []commitMatch) []commitMatch {
	if len(matches) == 0 {
		return matches
	}
	resolved := make([]commitMatch, len(matches))
	copy(resolved, matches)
	cache := make(map[string]string)
	for i := range resolved {
		if resolved[i].FullHash != "" || len(resolved[i].Hash) == 40 {
			if resolved[i].FullHash == "" {
				resolved[i].FullHash = resolved[i].Hash
			}
			continue
		}
		dir := commitResolveDir(resolved[i])
		if dir == "" {
			continue
		}
		key := dir + "|" + resolved[i].Hash
		full, ok := cache[key]
		if !ok {
			full = resolveCommitFullHash(dir, resolved[i].Hash)
			cache[key] = full
		}
		if full != "" {
			resolved[i].FullHash = full
		}
	}
	return resolved
}

func commitResolveDir(match commitMatch) string {
	dir := gitCOptionDir(match.Command, match.CWD)
	if dir != "" {
		return dir
	}
	return match.CWD
}

func gitCOptionDir(command, cwd string) string {
	fields := splitCommandFields(command)
	resolvedDir := ""
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "-C" {
			continue
		}
		dir := fields[i+1]
		if dir == "" {
			return ""
		}
		if filepath.IsAbs(dir) || cwd == "" {
			dir = filepath.Clean(dir)
		} else {
			dir = filepath.Clean(filepath.Join(cwd, dir))
		}
		if resolvedDir != "" && resolvedDir != dir {
			return ""
		}
		resolvedDir = dir
	}
	return resolvedDir
}

func splitCommandFields(command string) []string {
	var fields []string
	var builder strings.Builder
	var quote rune
	escaped := false
	for _, r := range command {
		if escaped {
			builder.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			builder.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if builder.Len() > 0 {
				fields = append(fields, builder.String())
				builder.Reset()
			}
			continue
		}
		builder.WriteRune(r)
	}
	if builder.Len() > 0 {
		fields = append(fields, builder.String())
	}
	return fields
}

func resolveCommitFullHash(dir, hash string) string {
	ctx, cancel := context.WithTimeout(context.Background(), commitResolveTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", hash+"^{commit}")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	candidates := extractCommitHashesFromOutput(string(output))
	for _, candidate := range candidates {
		if len(candidate) == 40 {
			return candidate
		}
	}
	return ""
}

func extractCommitHashesFromOutput(output string) []string {
	var hashes []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hash := ""
		if wholeCommitHashPattern.MatchString(line) {
			hash = strings.ToLower(line)
		} else if matches := leadingCommitHashPattern.FindStringSubmatch(line); len(matches) == 2 {
			hash = strings.ToLower(matches[1])
		}
		if hash == "" || seen[hash] {
			continue
		}
		seen[hash] = true
		hashes = append(hashes, hash)
	}
	return hashes
}

func dedupeCommitMatches(matches []commitMatch) []commitMatch {
	if len(matches) < 2 {
		return matches
	}
	seen := make(map[string]bool, len(matches))
	deduped := make([]commitMatch, 0, len(matches))
	for _, match := range matches {
		key := match.Hash + "|" + match.CallID
		if match.CallID == "" {
			key = match.Hash + "|" + match.Timestamp + "|" + match.CWD + "|" + match.Command
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, match)
	}
	return deduped
}

func commitMatchHasQuery(match commitMatch, query string) bool {
	hash := strings.ToLower(match.Hash)
	fullHash := strings.ToLower(match.FullHash)
	if hash != "" && (hash == query || strings.HasPrefix(hash, query) || strings.HasPrefix(query, hash)) {
		return true
	}
	if fullHash != "" && (fullHash == query || strings.HasPrefix(fullHash, query) || strings.HasPrefix(query, fullHash)) {
		return true
	}
	return false
}

func commitIndexReady(manager indexManager, state indexState) bool {
	for _, meta := range state.Sessions {
		if meta.CommitIndexFile == "" {
			return false
		}
		if !fileExists(filepath.Join(manager.StorageDir, meta.CommitIndexFile)) {
			return false
		}
	}
	return true
}

func commitLookupReady(manager indexManager, state indexState) bool {
	return commitIndexReady(manager, state) && fileExists(commitLookupPath(manager))
}

func countIndexedCommitRefs(state indexState) int {
	total := 0
	for _, meta := range state.Sessions {
		total += meta.CommitCount
	}
	return total
}

func countFullCommitRefs(state indexState) int {
	total := 0
	for _, meta := range state.Sessions {
		total += meta.FullCommitCount
	}
	return total
}

func countFullCommitMatches(matches []commitMatch) int {
	total := 0
	for _, match := range matches {
		if match.FullHash != "" {
			total++
		}
	}
	return total
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

func commitLookupPath(manager indexManager) string {
	if manager.CommitLookupPath != "" {
		return manager.CommitLookupPath
	}
	return filepath.Join(manager.StorageDir, commitLookupFileName)
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
