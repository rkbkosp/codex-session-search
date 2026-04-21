package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultRoot         = "~/.codex"
	defaultLimit        = 10
	defaultSnippets     = 2
	defaultView         = "compact"
	maxScannerLineBytes = 64 * 1024 * 1024
)

var relativeWindowPattern = regexp.MustCompile(`(?i)^\s*(\d+)\s*(mon|mons|month|months|d|day|days|h|hr|hrs|hour|hours|min|mins|minute|minutes)\s*$`)

type config struct {
	Query         string
	From          string
	To            string
	Last          string
	LastSince     time.Time
	LastUntil     time.Time
	Limit         int
	Snippets      int
	Root          string
	JSON          bool
	CaseSensitive bool
	Role          string
	View          string
}

type indexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
	UpdatedAt  string `json:"updated_at"`
}

type historyEntry struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

type eventEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

type responseItem struct {
	Type    string                   `json:"type"`
	Role    string                   `json:"role"`
	Content []map[string]interface{} `json:"content"`
}

type sessionFile struct {
	Path      string
	Date      string
	StartedAt time.Time
}

type message struct {
	Role      string `json:"role"`
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text"`
}

type snippet struct {
	Before *message `json:"before,omitempty"`
	Match  message  `json:"match"`
	After  *message `json:"after,omitempty"`
}

type result struct {
	ID           string    `json:"id"`
	Title        string    `json:"title,omitempty"`
	UpdatedAt    string    `json:"updated_at,omitempty"`
	Date         string    `json:"date,omitempty"`
	StartedAt    string    `json:"started_at,omitempty"`
	CWD          string    `json:"cwd,omitempty"`
	Path         string    `json:"path"`
	MatchCount   int       `json:"match_count"`
	TitleMatched bool      `json:"title_matched"`
	Resume       string    `json:"resume"`
	Snippets     []snippet `json:"snippets,omitempty"`
}

type jsonOutput struct {
	Query           string         `json:"query"`
	RoleFilter      string         `json:"role_filter,omitempty"`
	ScannedSessions int            `json:"scanned_sessions"`
	Elapsed         string         `json:"elapsed"`
	Results         []result       `json:"results"`
	Warnings        []string       `json:"warnings,omitempty"`
	DateRange       *dateRangeOut  `json:"date_range,omitempty"`
	LastWindow      *lastWindowOut `json:"last_window,omitempty"`
}

type dateRangeOut struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

type lastWindowOut struct {
	Expression string `json:"expression"`
	Since      string `json:"since"`
	Until      string `json:"until"`
}

type pendingSnippet struct {
	Before *message
	Match  message
}

type outputTheme struct {
	Color bool
}

func main() {
	if handled, code := handleSubcommand(os.Args[1:]); handled {
		os.Exit(code)
	}

	cfg, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
		printUsage(os.Stderr)
		os.Exit(2)
	}

	if code, ok := runIndexedSearch(cfg); ok {
		os.Exit(code)
	}

	runRawSearch(cfg)
}

func runIndexedSearch(cfg config) (int, bool) {
	manager, err := newIndexManager(cfg.Root)
	if err != nil {
		return 0, false
	}
	start := time.Now()
	results, warnings, scanned, err := searchWithIndex(manager, cfg)
	if err != nil {
		return 0, false
	}
	elapsed := time.Since(start)
	sortResults(results)
	if cfg.Limit > 0 && len(results) > cfg.Limit {
		results = results[:cfg.Limit]
	}
	if cfg.JSON {
		out := jsonOutput{
			Query:           cfg.Query,
			RoleFilter:      cfg.Role,
			ScannedSessions: scanned,
			Elapsed:         elapsed.Round(time.Millisecond).String(),
			Results:         results,
			Warnings:        warnings,
		}
		if cfg.From != "" || cfg.To != "" {
			out.DateRange = &dateRangeOut{From: cfg.From, To: cfg.To}
		}
		if cfg.Last != "" {
			out.LastWindow = &lastWindowOut{
				Expression: cfg.Last,
				Since:      cfg.LastSince.Format(time.RFC3339),
				Until:      cfg.LastUntil.Format(time.RFC3339),
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "error: write json: %v\n", err)
			return 1, true
		}
		if len(results) == 0 {
			return 1, true
		}
		return 0, true
	}
	printText(results, warnings, scanned, elapsed, cfg)
	if len(results) == 0 {
		return 1, true
	}
	return 0, true
}

func runRawSearch(cfg config) {
	index, err := loadSessionIndex(filepath.Join(cfg.Root, "session_index.jsonl"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load session index: %v\n", err)
		os.Exit(1)
	}

	history, err := loadHistory(filepath.Join(cfg.Root, "history.jsonl"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load history: %v\n", err)
		os.Exit(1)
	}

	from, to := effectiveDateRange(cfg)
	files, err := collectSessionFiles(filepath.Join(cfg.Root, "sessions"), from, to, cfg.LastSince, cfg.LastUntil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: collect session files: %v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	results, warnings := searchSessions(files, cfg, index, history)
	elapsed := time.Since(start)
	sortResults(results)
	if cfg.Limit > 0 && len(results) > cfg.Limit {
		results = results[:cfg.Limit]
	}

	if cfg.JSON {
		out := jsonOutput{
			Query:           cfg.Query,
			RoleFilter:      cfg.Role,
			ScannedSessions: len(files),
			Elapsed:         elapsed.Round(time.Millisecond).String(),
			Results:         results,
			Warnings:        warnings,
		}
		if cfg.From != "" || cfg.To != "" {
			out.DateRange = &dateRangeOut{From: cfg.From, To: cfg.To}
		}
		if cfg.Last != "" {
			out.LastWindow = &lastWindowOut{
				Expression: cfg.Last,
				Since:      cfg.LastSince.Format(time.RFC3339),
				Until:      cfg.LastUntil.Format(time.RFC3339),
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "error: write json: %v\n", err)
			os.Exit(1)
		}
		if len(results) == 0 {
			os.Exit(1)
		}
		return
	}

	printText(results, warnings, len(files), elapsed, cfg)
	if len(results) == 0 {
		os.Exit(1)
	}
}

func parseArgs(args []string) (config, error) {
	cfg := config{
		Limit:    defaultLimit,
		Snippets: defaultSnippets,
		Root:     defaultRoot,
		Role:     "all",
		View:     defaultView,
	}
	var queryParts []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			printUsage(os.Stdout)
			os.Exit(0)
		case "--from":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.From = value
			i = next
		case "--to":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.To = value
			i = next
		case "--on":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.From = value
			cfg.To = value
			i = next
		case "--last":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.Last = value
			i = next
		case "--limit", "-n":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			limit, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("%s requires an integer: %w", arg, err)
			}
			cfg.Limit = limit
			i = next
		case "--snippets":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			snippets, err := strconv.Atoi(value)
			if err != nil {
				return cfg, fmt.Errorf("%s requires an integer: %w", arg, err)
			}
			cfg.Snippets = snippets
			i = next
		case "--root":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.Root = value
			i = next
		case "--json":
			cfg.JSON = true
		case "--case-sensitive":
			cfg.CaseSensitive = true
		case "--role":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.Role = strings.ToLower(value)
			i = next
		case "--view":
			value, next, err := nextValue(args, i, arg)
			if err != nil {
				return cfg, err
			}
			cfg.View = strings.ToLower(value)
			i = next
		case "--assistant-only":
			cfg.Role = "assistant"
		case "--user-only":
			cfg.Role = "user"
		default:
			if strings.HasPrefix(arg, "-") {
				return cfg, fmt.Errorf("unknown flag: %s", arg)
			}
			queryParts = append(queryParts, arg)
		}
	}

	if len(queryParts) == 0 {
		return cfg, errors.New("missing search query")
	}
	if cfg.Limit < 0 {
		return cfg, errors.New("--limit must be >= 0")
	}
	if cfg.Snippets < 0 {
		return cfg, errors.New("--snippets must be >= 0")
	}
	if !validRole(cfg.Role) {
		return cfg, errors.New("--role must be one of: all, assistant, user")
	}
	if !validView(cfg.View) {
		return cfg, errors.New("--view must be one of: compact, full")
	}
	if cfg.Last != "" && (cfg.From != "" || cfg.To != "") {
		return cfg, errors.New("--last cannot be combined with --from, --to, or --on")
	}
	if err := validateDate(cfg.From); err != nil {
		return cfg, fmt.Errorf("invalid --from: %w", err)
	}
	if err := validateDate(cfg.To); err != nil {
		return cfg, fmt.Errorf("invalid --to: %w", err)
	}
	if cfg.From != "" && cfg.To != "" && cfg.From > cfg.To {
		return cfg, errors.New("--from cannot be later than --to")
	}
	if cfg.Last != "" {
		since, until, err := parseRelativeWindow(cfg.Last, time.Now())
		if err != nil {
			return cfg, fmt.Errorf("invalid --last: %w", err)
		}
		cfg.Last = strings.ToLower(strings.TrimSpace(cfg.Last))
		cfg.LastSince = since
		cfg.LastUntil = until
	}
	root, err := expandPath(cfg.Root)
	if err != nil {
		return cfg, fmt.Errorf("resolve --root: %w", err)
	}
	cfg.Root = root
	cfg.Query = strings.Join(queryParts, " ")
	return cfg, nil
}

func nextValue(args []string, index int, flag string) (string, int, error) {
	next := index + 1
	if next >= len(args) {
		return "", index, fmt.Errorf("%s requires a value", flag)
	}
	return args[next], next, nil
}

func validateDate(value string) error {
	if value == "" {
		return nil
	}
	_, err := time.Parse("2006-01-02", value)
	return err
}

func parseRelativeWindow(raw string, now time.Time) (time.Time, time.Time, error) {
	matches := relativeWindowPattern.FindStringSubmatch(raw)
	if len(matches) != 3 {
		return time.Time{}, time.Time{}, errors.New("expected formats like 3d, 3mon, 3h, 90min")
	}
	amount, err := strconv.Atoi(matches[1])
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if amount <= 0 {
		return time.Time{}, time.Time{}, errors.New("value must be greater than 0")
	}
	unit := strings.ToLower(matches[2])
	switch unit {
	case "mon", "mons", "month", "months":
		return now.AddDate(0, -amount, 0), now, nil
	case "d", "day", "days":
		return now.AddDate(0, 0, -amount), now, nil
	case "h", "hr", "hrs", "hour", "hours":
		return now.Add(-time.Duration(amount) * time.Hour), now, nil
	case "min", "mins", "minute", "minutes":
		return now.Add(-time.Duration(amount) * time.Minute), now, nil
	default:
		return time.Time{}, time.Time{}, errors.New("unsupported time unit")
	}
}

func expandPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func loadSessionIndex(path string) (map[string]indexEntry, error) {
	entries := make(map[string]indexEntry)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return entries, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var entry indexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		if entry.ID == "" {
			continue
		}
		entries[entry.ID] = entry
	}
	return entries, scanner.Err()
}

func loadHistory(path string) (map[string][]string, error) {
	entries := make(map[string][]string)
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return entries, nil
		}
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var entry historyEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		text := normalizeWhitespace(entry.Text)
		if entry.SessionID == "" || text == "" {
			continue
		}
		existing := entries[entry.SessionID]
		if len(existing) > 0 && existing[len(existing)-1] == text {
			continue
		}
		if len(existing) < 3 {
			entries[entry.SessionID] = append(existing, text)
		}
	}
	return entries, scanner.Err()
}

func collectSessionFiles(root, from, to string, since, until time.Time) ([]sessionFile, error) {
	var files []sessionFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		date := extractDateFromPath(root, path)
		if !dateWithinRange(date, from, to) {
			return nil
		}
		startedAt := extractStartTimeFromFilename(path)
		if !timeWithinRange(startedAt, since, until) {
			return nil
		}
		files = append(files, sessionFile{
			Path:      path,
			Date:      date,
			StartedAt: startedAt,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].StartedAt.IsZero() && !files[j].StartedAt.IsZero() && !files[i].StartedAt.Equal(files[j].StartedAt) {
			return files[i].StartedAt.After(files[j].StartedAt)
		}
		if files[i].Date == files[j].Date {
			return files[i].Path > files[j].Path
		}
		return files[i].Date > files[j].Date
	})
	return files, nil
}

func extractStartTimeFromFilename(path string) time.Time {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if !strings.HasPrefix(base, "rollout-") {
		return time.Time{}
	}
	body := strings.TrimPrefix(base, "rollout-")
	if len(body) <= 37 {
		return time.Time{}
	}
	timestampPart := body[:len(body)-37]
	startedAt, err := time.ParseInLocation("2006-01-02T15-04-05", timestampPart, time.Local)
	if err != nil {
		return time.Time{}
	}
	return startedAt
}

func extractDateFromPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) < 4 {
		return ""
	}
	year, month, day := parts[0], parts[1], parts[2]
	if len(year) != 4 || len(month) != 2 || len(day) != 2 {
		return ""
	}
	return year + "-" + month + "-" + day
}

func dateWithinRange(date, from, to string) bool {
	if date == "" {
		return from == "" && to == ""
	}
	if from != "" && date < from {
		return false
	}
	if to != "" && date > to {
		return false
	}
	return true
}

func timeWithinRange(startedAt, since, until time.Time) bool {
	if since.IsZero() && until.IsZero() {
		return true
	}
	if startedAt.IsZero() {
		return true
	}
	if !since.IsZero() && startedAt.Before(since) {
		return false
	}
	if !until.IsZero() && startedAt.After(until) {
		return false
	}
	return true
}

func effectiveDateRange(cfg config) (string, string) {
	if cfg.Last == "" {
		return cfg.From, cfg.To
	}
	return cfg.LastSince.In(time.Local).Format("2006-01-02"), cfg.LastUntil.In(time.Local).Format("2006-01-02")
}

func searchSessions(files []sessionFile, cfg config, index map[string]indexEntry, history map[string][]string) ([]result, []string) {
	var results []result
	var warnings []string
	for _, file := range files {
		res, err := searchFile(file, cfg, index, history)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", file.Path, err))
			continue
		}
		if res == nil {
			continue
		}
		results = append(results, *res)
	}
	return results, warnings
}

func searchFile(file sessionFile, cfg config, index map[string]indexEntry, history map[string][]string) (*result, error) {
	id := extractSessionID(file.Path)
	entry := index[id]
	res := &result{
		ID:        id,
		Title:     normalizeWhitespace(entry.ThreadName),
		UpdatedAt: entry.UpdatedAt,
		Date:      file.Date,
		Path:      file.Path,
		Resume:    "codex resume " + id,
	}

	matches := makeMatcher(cfg.Query, cfg.CaseSensitive)
	if res.Title != "" && matches(res.Title) {
		res.TitleMatched = true
	}

	handle, err := os.Open(file.Path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()

	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)

	var prev *message
	var pending []pendingSnippet
	for scanner.Scan() {
		var env eventEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			continue
		}
		switch env.Type {
		case "session_meta":
			var meta sessionMeta
			if err := json.Unmarshal(env.Payload, &meta); err == nil {
				if meta.ID != "" {
					if meta.ID != res.ID {
						if refreshed, ok := index[meta.ID]; ok {
							if res.Title == "" {
								res.Title = normalizeWhitespace(refreshed.ThreadName)
							}
							if res.UpdatedAt == "" {
								res.UpdatedAt = refreshed.UpdatedAt
							}
						}
					}
					res.ID = meta.ID
					res.Resume = "codex resume " + meta.ID
				}
				if meta.CWD != "" {
					res.CWD = meta.CWD
				}
				if meta.Timestamp != "" {
					res.StartedAt = meta.Timestamp
				}
			}
		case "response_item":
			msg := extractMessage(env.Timestamp, env.Payload)
			if msg == nil || !searchableRole(msg.Role, cfg.Role) {
				continue
			}
			if len(pending) > 0 {
				after := cloneMessage(msg)
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
						Match:  *msg,
					})
				}
			}
			prev = cloneMessage(msg)
		}
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

	if res.Title == "" {
		res.Title = fallbackTitle(history[res.ID])
	}
	if res.MatchCount == 0 && res.TitleMatched && len(res.Snippets) == 0 {
		if fallback := fallbackSnippet(history[res.ID]); fallback != nil {
			res.Snippets = append(res.Snippets, *fallback)
		}
	}
	if res.MatchCount == 0 && !res.TitleMatched {
		return nil, nil
	}
	return res, nil
}

func extractMessage(timestamp string, payload json.RawMessage) *message {
	var item responseItem
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil
	}
	if item.Type != "message" || item.Role == "" {
		return nil
	}
	text := extractContentText(item.Content)
	if text == "" {
		return nil
	}
	text = cleanMessageText(item.Role, text)
	if text == "" {
		return nil
	}
	return &message{
		Role:      item.Role,
		Timestamp: timestamp,
		Text:      text,
	}
}

func extractContentText(content []map[string]interface{}) string {
	var parts []string
	for _, item := range content {
		collectText(item, &parts)
	}
	if len(parts) == 0 {
		return ""
	}
	return normalizeWhitespace(strings.Join(parts, " "))
}

func collectText(value interface{}, parts *[]string) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if text, ok := typed["text"].(string); ok {
			text = normalizeWhitespace(text)
			if text != "" {
				*parts = append(*parts, text)
			}
		}
		for _, child := range typed {
			collectText(child, parts)
		}
	case []interface{}:
		for _, item := range typed {
			collectText(item, parts)
		}
	}
}

func validRole(role string) bool {
	return role == "all" || role == "assistant" || role == "user"
}

func validView(view string) bool {
	return view == "compact" || view == "full"
}

func searchableRole(role, wanted string) bool {
	switch wanted {
	case "assistant":
		return role == "assistant"
	case "user":
		return role == "user"
	default:
		return role == "user" || role == "assistant"
	}
}

func fallbackTitle(texts []string) string {
	if len(texts) == 0 {
		return "(untitled session)"
	}
	return shorten(texts[0], "", false, 80)
}

func fallbackSnippet(texts []string) *snippet {
	if len(texts) == 0 {
		return nil
	}
	return &snippet{
		Match: message{
			Role: "user",
			Text: texts[0],
		},
	}
}

func makeMatcher(query string, caseSensitive bool) func(string) bool {
	if caseSensitive {
		return func(text string) bool {
			return strings.Contains(text, query)
		}
	}
	queryLower := strings.ToLower(query)
	return func(text string) bool {
		return strings.Contains(strings.ToLower(text), queryLower)
	}
}

func sortResults(results []result) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].TitleMatched != results[j].TitleMatched {
			return results[i].TitleMatched
		}
		if results[i].MatchCount != results[j].MatchCount {
			return results[i].MatchCount > results[j].MatchCount
		}
		if results[i].UpdatedAt != results[j].UpdatedAt {
			return results[i].UpdatedAt > results[j].UpdatedAt
		}
		if results[i].Date != results[j].Date {
			return results[i].Date > results[j].Date
		}
		return results[i].ID > results[j].ID
	})
}

func cloneMessage(msg *message) *message {
	if msg == nil {
		return nil
	}
	copy := *msg
	return &copy
}

func normalizeWhitespace(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func cleanMessageText(role, text string) string {
	text = normalizeWhitespace(text)
	if role != "user" {
		return text
	}
	markers := []string{
		"## My request for Codex:",
		"My request for Codex:",
	}
	for _, marker := range markers {
		if idx := strings.Index(text, marker); idx >= 0 {
			text = strings.TrimSpace(text[idx+len(marker):])
		}
	}
	if strings.HasPrefix(text, "# AGENTS.md instructions") {
		return ""
	}
	if strings.HasPrefix(text, "Existing file:") {
		return ""
	}
	if strings.HasPrefix(text, "<turn_aborted>") {
		return ""
	}
	return text
}

func extractSessionID(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if len(base) >= 36 {
		candidate := base[len(base)-36:]
		if strings.HasPrefix(candidate, "019") {
			return candidate
		}
	}
	return base
}

func printText(results []result, warnings []string, scanned int, elapsed time.Duration, cfg config) {
	theme := detectOutputTheme()
	if len(results) == 0 {
		fmt.Printf("%s for %q. Scanned %d sessions in %s.\n", style(theme, "red+bold", "No matches"), cfg.Query, scanned, elapsed.Round(time.Millisecond))
		printDateRange(cfg)
		printLastWindow(cfg)
		printRoleFilter(cfg)
		printWarnings(warnings)
		return
	}

	fmt.Printf("%s %d matching sessions for %q. Scanned %d sessions in %s.\n", style(theme, "green+bold", "Found"), len(results), cfg.Query, scanned, elapsed.Round(time.Millisecond))
	printDateRange(cfg)
	printLastWindow(cfg)
	printRoleFilter(cfg)
	fmt.Println()
	if cfg.View == "compact" {
		printCompactResults(results, cfg, theme)
		printWarnings(warnings)
		return
	}
	printFullResults(results, cfg, theme)
	printWarnings(warnings)
}

func printCompactResults(results []result, cfg config, theme outputTheme) {
	for i, res := range results {
		title := highlightText(nonEmpty(res.Title, "(untitled session)"), cfg.Query, cfg.CaseSensitive, theme)
		fmt.Printf("%s %s\n", style(theme, "cyan+bold", fmt.Sprintf("[%d]", i+1)), style(theme, "bold", title))
		fmt.Printf("    %s\n", style(theme, "dim", compactMetaLine(res)))
		if snip := firstSnippet(res); snip != nil {
			role, text := snippetPreview(*snip, cfg, theme)
			fmt.Printf("    %s %s\n", style(theme, "yellow+bold", padRole(role)), text)
		}
		fmt.Printf("    %s %s\n", style(theme, "dim", "resume"), style(theme, "blue", res.Resume))
		fmt.Println()
	}
}

func printFullResults(results []result, cfg config, theme outputTheme) {
	for i, res := range results {
		fmt.Printf("%s %s\n", style(theme, "cyan+bold", fmt.Sprintf("[%d]", i+1)), style(theme, "bold", highlightText(nonEmpty(res.Title, "(untitled session)"), cfg.Query, cfg.CaseSensitive, theme)))
		fmt.Printf("    %s\n", style(theme, "dim", "id: "+res.ID))
		var summary []string
		if res.Date != "" {
			summary = append(summary, "date: "+res.Date)
		}
		if res.UpdatedAt != "" {
			summary = append(summary, "updated: "+trimTimestamp(res.UpdatedAt))
		}
		if res.MatchCount > 0 {
			summary = append(summary, fmt.Sprintf("hits: %d", res.MatchCount))
		}
		if res.TitleMatched {
			summary = append(summary, "title-hit: yes")
		}
		if len(summary) > 0 {
			fmt.Printf("    %s\n", style(theme, "dim", strings.Join(summary, " | ")))
		}
		if res.CWD != "" {
			fmt.Printf("    %s %s\n", style(theme, "dim", "cwd"), res.CWD)
		}
		fmt.Printf("    %s %s\n", style(theme, "dim", "resume"), style(theme, "blue", res.Resume))
		for idx, snip := range res.Snippets {
			fmt.Printf("    %s %d\n", style(theme, "magenta+bold", "context"), idx+1)
			if snip.Before != nil {
				fmt.Printf("      %s %s\n", style(theme, "dim", "["+snip.Before.Role+"]"), highlightText(shorten(snip.Before.Text, cfg.Query, cfg.CaseSensitive, 180), cfg.Query, cfg.CaseSensitive, theme))
			}
			fmt.Printf("      %s %s\n", style(theme, "yellow+bold", "["+snip.Match.Role+"]"), highlightText(shorten(snip.Match.Text, cfg.Query, cfg.CaseSensitive, 180), cfg.Query, cfg.CaseSensitive, theme))
			if snip.After != nil {
				fmt.Printf("      %s %s\n", style(theme, "dim", "["+snip.After.Role+"]"), highlightText(shorten(snip.After.Text, cfg.Query, cfg.CaseSensitive, 180), cfg.Query, cfg.CaseSensitive, theme))
			}
		}
		fmt.Println()
	}
}

func printDateRange(cfg config) {
	if cfg.From == "" && cfg.To == "" {
		return
	}
	fmt.Printf("Date range: %s -> %s\n", nonEmpty(cfg.From, "*"), nonEmpty(cfg.To, "*"))
}

func printLastWindow(cfg config) {
	if cfg.Last == "" {
		return
	}
	fmt.Printf("Last window: %s | since: %s | until: %s\n", cfg.Last, cfg.LastSince.Format(time.RFC3339), cfg.LastUntil.Format(time.RFC3339))
}

func printRoleFilter(cfg config) {
	if cfg.Role == "" || cfg.Role == "all" {
		return
	}
	fmt.Printf("Roles: %s\n", cfg.Role)
}

func printWarnings(warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
}

func detectOutputTheme() outputTheme {
	if os.Getenv("NO_COLOR") != "" {
		return outputTheme{}
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return outputTheme{}
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return outputTheme{}
	}
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		return outputTheme{}
	}
	return outputTheme{Color: true}
}

func style(theme outputTheme, kind, text string) string {
	if !theme.Color || text == "" {
		return text
	}
	switch kind {
	case "bold":
		return "\033[1m" + text + "\033[0m"
	case "dim":
		return "\033[2m" + text + "\033[0m"
	case "blue":
		return "\033[34m" + text + "\033[0m"
	case "cyan+bold":
		return "\033[1;36m" + text + "\033[0m"
	case "green+bold":
		return "\033[1;32m" + text + "\033[0m"
	case "yellow+bold":
		return "\033[1;33m" + text + "\033[0m"
	case "magenta+bold":
		return "\033[1;35m" + text + "\033[0m"
	case "red+bold":
		return "\033[1;31m" + text + "\033[0m"
	default:
		return text
	}
}

func highlightText(text, query string, caseSensitive bool, theme outputTheme) string {
	if !theme.Color || query == "" || text == "" {
		return text
	}
	pattern := regexp.QuoteMeta(query)
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return text
	}
	indexes := re.FindAllStringIndex(text, -1)
	if len(indexes) == 0 {
		return text
	}
	var builder strings.Builder
	last := 0
	for _, pair := range indexes {
		if pair[0] < last {
			continue
		}
		builder.WriteString(text[last:pair[0]])
		builder.WriteString(style(theme, "yellow+bold", text[pair[0]:pair[1]]))
		last = pair[1]
	}
	builder.WriteString(text[last:])
	return builder.String()
}

func compactMetaLine(res result) string {
	var parts []string
	parts = append(parts, truncateID(res.ID))
	if res.Date != "" {
		parts = append(parts, res.Date)
	}
	if res.CWD != "" {
		parts = append(parts, pathTail(res.CWD))
	}
	if res.MatchCount > 0 {
		parts = append(parts, fmt.Sprintf("hits:%d", res.MatchCount))
	}
	if res.TitleMatched {
		parts = append(parts, "title")
	}
	return strings.Join(parts, " | ")
}

func firstSnippet(res result) *snippet {
	if len(res.Snippets) == 0 {
		return nil
	}
	snip := res.Snippets[0]
	return &snip
}

func snippetPreview(snip snippet, cfg config, theme outputTheme) (string, string) {
	text := shorten(snip.Match.Text, cfg.Query, cfg.CaseSensitive, 140)
	return snip.Match.Role, highlightText(text, cfg.Query, cfg.CaseSensitive, theme)
}

func padRole(role string) string {
	switch role {
	case "assistant":
		return "assistant"
	case "user":
		return "user     "
	default:
		return role
	}
}

func truncateID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12] + "..."
}

func pathTail(path string) string {
	base := filepath.Base(path)
	if base == "." || base == string(os.PathSeparator) || base == "" {
		return path
	}
	return base
}

func trimTimestamp(value string) string {
	if value == "" {
		return ""
	}
	return strings.TrimSuffix(value, "Z")
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func shorten(text, query string, caseSensitive bool, width int) string {
	if width <= 0 || text == "" {
		return text
	}
	if utf8.RuneCountInString(text) <= width {
		return text
	}

	index := matchIndex(text, query, caseSensitive)
	if index < 0 {
		return trimWindow(text, 0, byteEndAtRunes(text, 0, width)) + "..."
	}

	start := index - 80
	if start < 0 {
		start = 0
	}
	start = alignToRuneStart(text, start)
	end := byteEndAtRunes(text, index, width)
	if end < len(text) {
		end = alignToRuneStart(text, end)
	}

	prefix := ""
	suffix := ""
	if start > 0 {
		prefix = "..."
	}
	if end < len(text) {
		suffix = "..."
	}
	return prefix + trimWindow(text, start, end) + suffix
}

func matchIndex(text, query string, caseSensitive bool) int {
	if query == "" {
		return -1
	}
	if caseSensitive {
		return strings.Index(text, query)
	}
	return strings.Index(strings.ToLower(text), strings.ToLower(query))
}

func byteEndAtRunes(text string, startByte, runeCount int) int {
	if startByte < 0 {
		startByte = 0
	}
	if startByte >= len(text) {
		return len(text)
	}
	end := startByte
	for i := 0; i < runeCount && end < len(text); i++ {
		_, size := utf8.DecodeRuneInString(text[end:])
		end += size
	}
	if end > len(text) {
		end = len(text)
	}
	return end
}

func alignToRuneStart(text string, index int) int {
	if index <= 0 {
		return 0
	}
	if index >= len(text) {
		return len(text)
	}
	for index > 0 && !utf8.RuneStart(text[index]) {
		index--
	}
	return index
}

func trimWindow(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(text) {
		end = len(text)
	}
	if start >= end {
		return ""
	}
	return text[start:end]
}

func printUsage(out *os.File) {
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  codex-session-search [flags] <query>")
	fmt.Fprintln(out, "  codex-session-search index refresh [--root PATH]")
	fmt.Fprintln(out, "  codex-session-search index status [--root PATH]")
	fmt.Fprintln(out, "  codex-session-search daemon install [--root PATH] [--interval 15s]")
	fmt.Fprintln(out, "  codex-session-search daemon start|stop|status|uninstall [--root PATH]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags:")
	fmt.Fprintln(out, "  --from YYYY-MM-DD     Inclusive start date")
	fmt.Fprintln(out, "  --to YYYY-MM-DD       Inclusive end date")
	fmt.Fprintln(out, "  --on YYYY-MM-DD       Search a single day")
	fmt.Fprintln(out, "  --last SPAN           Relative window like 3d, 3mon, 3h, 90min")
	fmt.Fprintln(out, "  --limit N             Max sessions to print (0 = all, default 10)")
	fmt.Fprintln(out, "  --snippets N          Max contexts per session (default 2)")
	fmt.Fprintln(out, "  --root PATH           Codex home directory (default ~/.codex)")
	fmt.Fprintln(out, "  --json                Emit JSON")
	fmt.Fprintln(out, "  --case-sensitive      Use case-sensitive matching")
	fmt.Fprintln(out, "  --role VALUE          all | assistant | user (default all)")
	fmt.Fprintln(out, "  --view VALUE          compact | full (default compact)")
	fmt.Fprintln(out, "  --assistant-only      Shortcut for --role assistant")
	fmt.Fprintln(out, "  --user-only           Shortcut for --role user")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  codex-session-search \"什么是Go语言\"")
	fmt.Fprintln(out, "  codex-session-search --role assistant \"SQLite\"")
	fmt.Fprintln(out, "  codex-session-search --last 3d \"SRT\"")
	fmt.Fprintln(out, "  codex-session-search --last 3h --assistant-only \"上下文\"")
	fmt.Fprintln(out, "  codex-session-search --view full --limit 5 \"drama_workspace\"")
	fmt.Fprintln(out, "  codex-session-search --from 2026-04-01 --to 2026-04-20 \"renderwarden\"")
	fmt.Fprintln(out, "  codex-session-search --on 2026-04-20 --limit 5 \"SRT\"")
	fmt.Fprintln(out, "  codex-session-search daemon install --interval 15s")
}
