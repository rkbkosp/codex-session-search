package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	tuiDefaultLimit    = 50
	tuiDefaultSnippets = 3
	tuiPreviewMessages = 6
)

type tuiSearchMode int

const (
	tuiSearchText tuiSearchMode = iota
	tuiSearchCommit
)

type tuiLaunchMode int

const (
	tuiLaunchCLI tuiLaunchMode = iota
	tuiLaunchApp
)

type tuiScreen int

const (
	tuiScreenSearch tuiScreen = iota
	tuiScreenResults
)

type tuiModel struct {
	root string

	width  int
	height int

	screen tuiScreen
	mode   tuiSearchMode
	launch tuiLaunchMode

	searchInput  textinput.Model
	activeMode   tuiSearchMode
	activeTerm   string
	updateNotice updateNotice

	results  []result
	warnings []string
	scanned  int
	elapsed  time.Duration
	selected int

	loading  bool
	searchID int
	errText  string
	status   string
}

type tuiSearchDoneMsg struct {
	ID       int
	Query    string
	Mode     tuiSearchMode
	Results  []result
	Warnings []string
	Scanned  int
	Elapsed  time.Duration
	Err      error
}

type tuiCopyDoneMsg struct {
	Command string
	Err     error
}

type tuiLaunchDoneMsg struct {
	Mode      tuiLaunchMode
	SessionID string
	Err       error
}

type tuiUpdateNoticeMsg struct {
	Notice updateNotice
	Err    error
}

var (
	tuiTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86"))
	tuiDimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244"))
	tuiErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("203")).
			Bold(true)
	tuiStatusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("250"))
	tuiInputStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			Padding(0, 1)
	tuiModeStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Foreground(lipgloss.Color("250"))
	tuiModeActiveStyle = lipgloss.NewStyle().
				Padding(0, 1).
				Foreground(lipgloss.Color("230")).
				Background(lipgloss.Color("62")).
				Bold(true)
	tuiPaneStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	tuiSelectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("230")).
				Background(lipgloss.Color("62")).
				Bold(true)
	tuiUserStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("81")).
			Bold(true)
	tuiAssistantStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("219")).
				Bold(true)
	tuiCommitStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("221")).
			Bold(true)
)

func runTUI(args []string) error {
	if !interactiveTerminal() {
		return errors.New("TUI requires an interactive terminal")
	}
	root, err := expandPath(defaultRoot)
	if err != nil {
		return err
	}
	model := newTUIModel(root, strings.TrimSpace(strings.Join(args, " ")))
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

func interactiveTerminal() bool {
	stdin, err := os.Stdin.Stat()
	if err != nil || stdin.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	stdout, err := os.Stdout.Stat()
	if err != nil || stdout.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return true
}

func newTUIModel(root, initialQuery string) tuiModel {
	input := newTUISearchInput(tuiSearchText, initialQuery)
	model := tuiModel{
		root:        root,
		width:       80,
		height:      24,
		screen:      tuiScreenSearch,
		mode:        tuiSearchText,
		activeMode:  tuiSearchText,
		searchInput: input,
		launch:      tuiLaunchCLI,
	}
	if initialQuery != "" {
		model.screen = tuiScreenResults
		model.loading = true
		model.searchID = 1
		model.activeTerm = initialQuery
		model.status = "Searching..."
		model.searchInput.Blur()
	} else {
		_ = model.searchInput.Focus()
	}
	if !isDevVersion(version) {
		notice, err := startupUpdateNotice(version)
		if err == nil {
			model.updateNotice = notice
		}
	}
	return model
}

func newTUISearchInput(mode tuiSearchMode, value string) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = mode.Placeholder()
	input.PlaceholderStyle = tuiDimStyle
	input.TextStyle = tuiStatusStyle
	input.Cursor.Style = lipgloss.NewStyle().Reverse(true)
	input.SetValue(value)
	input.CursorEnd()
	return input
}

func (m tuiModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.loading && m.searchID > 0 {
		cmds = append(cmds, m.searchCmd(m.searchID, m.activeTerm, m.activeMode))
	}
	if m.screen == tuiScreenSearch {
		cmds = append(cmds, m.searchInput.Cursor.BlinkCmd())
	}
	if shouldAutoCheckUpdates() {
		cmds = append(cmds, m.updateCheckCmd())
	}
	return tea.Batch(cmds...)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = tuiMax(40, msg.Width)
		m.height = tuiMax(12, msg.Height)
		return m, nil
	case tuiSearchDoneMsg:
		if msg.ID != m.searchID {
			return m, nil
		}
		m.loading = false
		m.results = msg.Results
		m.warnings = msg.Warnings
		m.scanned = msg.Scanned
		m.elapsed = msg.Elapsed
		m.errText = ""
		m.status = ""
		m.selected = 0
		m.activeMode = msg.Mode
		m.activeTerm = msg.Query
		if msg.Err != nil {
			m.errText = msg.Err.Error()
			m.status = "Search failed. Press Esc to edit."
		} else if len(msg.Results) == 0 {
			m.status = "No matches. Press Esc to edit."
		} else {
			m.status = fmt.Sprintf("Found %d sessions. Enter launches the highlighted session.", len(msg.Results))
		}
		return m, nil
	case tuiCopyDoneMsg:
		if msg.Err != nil {
			m.status = "Copy failed: " + msg.Err.Error()
		} else {
			m.status = "Copied: " + msg.Command
		}
		return m, nil
	case tuiLaunchDoneMsg:
		if msg.Err != nil {
			m.status = "Launch failed: " + msg.Err.Error()
		} else {
			m.status = fmt.Sprintf("Launched %s with %s.", msg.SessionID, msg.Mode.Label())
		}
		return m, nil
	case tuiUpdateNoticeMsg:
		if msg.Err != nil {
			m.updateNotice = updateNotice{
				Kind:  updateNoticeFailed,
				Error: msg.Err.Error(),
			}
			return m, nil
		}
		if msg.Notice.Kind != updateNoticeNone && msg.Notice.Kind != updateNoticeCurrent {
			m.updateNotice = msg.Notice
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		}
		if m.loading {
			if msg.String() == "q" && m.screen == tuiScreenResults {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.screen == tuiScreenResults {
			return m.updateResultsKey(msg)
		}
		return m.updateSearchKey(msg)
	}
	if m.screen == tuiScreenSearch && !m.loading {
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m tuiModel) updateSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.startSearch()
	case "tab", "shift+tab":
		m.mode = m.mode.Toggle()
		m.searchInput.Placeholder = m.mode.Placeholder()
		m.status = "Search mode: " + m.mode.Label()
		return m, nil
	case "esc":
		return m, tea.Quit
	}
	before := m.searchInput.Value()
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	if m.searchInput.Value() != before {
		m.status = ""
	}
	return m, cmd
}

func (m tuiModel) updateResultsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "esc":
		m.screen = tuiScreenSearch
		m.errText = ""
		m.status = "Edit search and press Enter."
		return m, m.searchInput.Focus()
	case "up", "k":
		m.selected = tuiMax(0, m.selected-1)
		m.status = ""
		return m, nil
	case "down", "j":
		m.selected = tuiMin(tuiMax(0, len(m.results)-1), m.selected+1)
		m.status = ""
		return m, nil
	case "tab", "shift+tab":
		m.launch = m.launch.Toggle()
		m.status = "Launch mode: " + m.launch.Label()
		return m, nil
	case "r":
		m.launch = tuiLaunchCLI
		m.status = "Launch mode: cli"
		return m, nil
	case "c":
		return m.copySelectedCommand()
	case "enter":
		return m.launchSelected()
	}
	return m, nil
}

func (m tuiModel) startSearch() (tea.Model, tea.Cmd) {
	query, err := normalizeTUIQuery(m.searchInput.Value(), m.mode)
	if err != nil {
		m.errText = ""
		m.status = err.Error()
		return m, nil
	}
	m.searchInput.SetValue(query)
	m.searchInput.CursorEnd()
	m.searchInput.Blur()
	m.activeTerm = query
	m.activeMode = m.mode
	m.screen = tuiScreenResults
	m.loading = true
	m.errText = ""
	m.status = "Searching..."
	m.results = nil
	m.warnings = nil
	m.scanned = 0
	m.elapsed = 0
	m.selected = 0
	m.searchID++
	return m, m.searchCmd(m.searchID, query, m.mode)
}

func (m tuiModel) searchCmd(id int, query string, mode tuiSearchMode) tea.Cmd {
	root := m.root
	return func() tea.Msg {
		start := time.Now()
		results, warnings, scanned, err := runTUISearch(root, query, mode)
		return tuiSearchDoneMsg{
			ID:       id,
			Query:    query,
			Mode:     mode,
			Results:  results,
			Warnings: warnings,
			Scanned:  scanned,
			Elapsed:  time.Since(start),
			Err:      err,
		}
	}
}

func (m tuiModel) updateCheckCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout+updateDownloadTimeout+updateVersionTimeout)
		defer cancel()
		notice, err := autoUpdateIfNeeded(ctx, releaseRepository)
		return tuiUpdateNoticeMsg{Notice: notice, Err: err}
	}
}

func (m tuiModel) copySelectedCommand() (tea.Model, tea.Cmd) {
	res, ok := m.selectedResult()
	if !ok {
		m.status = "No session selected."
		return m, nil
	}
	command := launchCommandString(res.ID, m.launch)
	m.status = "Copying: " + command
	return m, func() tea.Msg {
		return tuiCopyDoneMsg{
			Command: command,
			Err:     writeClipboard(command),
		}
	}
}

func (m tuiModel) launchSelected() (tea.Model, tea.Cmd) {
	res, ok := m.selectedResult()
	if !ok {
		m.status = "No session selected."
		return m, nil
	}
	m.status = "Launching: " + launchCommandString(res.ID, m.launch)
	if m.launch == tuiLaunchCLI {
		cmd := exec.Command("codex", "resume", res.ID)
		return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
			return tuiLaunchDoneMsg{Mode: tuiLaunchCLI, SessionID: res.ID, Err: err}
		})
	}
	return m, func() tea.Msg {
		cmd := deepLinkCommand(res.ID)
		output, err := cmd.CombinedOutput()
		if err != nil {
			err = commandError(cmd, output, err)
		}
		return tuiLaunchDoneMsg{Mode: tuiLaunchApp, SessionID: res.ID, Err: err}
	}
}

func (m tuiModel) selectedResult() (result, bool) {
	if len(m.results) == 0 {
		return result{}, false
	}
	index := tuiClamp(m.selected, 0, len(m.results)-1)
	return m.results[index], true
}

func (m tuiModel) View() string {
	width := tuiMax(40, m.width)
	height := tuiMax(12, m.height)
	title := lipgloss.PlaceHorizontal(width, lipgloss.Center, tuiTitleStyle.Render("codex-session-search"))
	search := m.renderSearchBar(width)
	footer := m.renderUpdateFooter(width)
	fixedHeight := lipgloss.Height(title) + lipgloss.Height(search) + lipgloss.Height(footer)
	bodyHeight := tuiMax(6, height-fixedHeight)
	var body string
	if m.screen == tuiScreenResults {
		body = m.renderResultsBody(width, bodyHeight)
	} else {
		body = m.renderSearchBody(width, bodyHeight)
	}
	parts := []string{title, search, body}
	if footer != "" {
		parts = append(parts, footer)
	}
	return lipgloss.NewStyle().Width(width).Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

func (m tuiModel) renderSearchBar(width int) string {
	modeLine := lipgloss.JoinHorizontal(lipgloss.Top,
		m.modePill(tuiSearchText),
		" ",
		m.modePill(tuiSearchCommit),
	)
	boxWidth := tuiMax(20, width-lipgloss.Width(modeLine)-6)
	valueWidth := tuiMax(1, boxWidth-tuiInputStyle.GetHorizontalFrameSize())
	input := m.searchInput
	input.Placeholder = m.mode.Placeholder()
	input.Width = valueWidth
	if m.screen == tuiScreenSearch && !m.loading {
		_ = input.Focus()
	} else {
		input.Blur()
	}
	box := tuiInputStyle.Width(boxWidth).Render(input.View())
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, lipgloss.JoinHorizontal(lipgloss.Center, modeLine, "  ", box))
}

func (m tuiModel) modePill(mode tuiSearchMode) string {
	label := mode.Label()
	if m.mode == mode {
		return tuiModeActiveStyle.Render(label)
	}
	return tuiModeStyle.Render(label)
}

func (m tuiModel) renderSearchBody(width, height int) string {
	var lines []string
	lines = append(lines, "")
	if m.status != "" {
		lines = append(lines, tuiStatusStyle.Render(m.status))
	}
	lines = append(lines, tuiDimStyle.Render("Enter search | Tab mode | Left/Right cursor | Esc quit"))
	content := strings.Join(lines, "\n")
	return lipgloss.NewStyle().Width(width).Height(height).Align(lipgloss.Center).Render(content)
}

func (m tuiModel) renderResultsBody(width, height int) string {
	status := m.renderResultsStatus(width)
	remaining := tuiMax(5, height-lipgloss.Height(status))
	if m.loading {
		content := "\n" + tuiStatusStyle.Render("Searching "+m.activeMode.Label()+" for "+fmt.Sprintf("%q", m.activeTerm)+"...")
		return lipgloss.NewStyle().Width(width).Height(height).Align(lipgloss.Center).Render(content)
	}
	if m.errText != "" {
		content := "\n" + tuiErrorStyle.Render(m.errText) + "\n" + tuiDimStyle.Render("Esc edit search | q quit")
		return lipgloss.NewStyle().Width(width).Height(height).Align(lipgloss.Center).Render(content)
	}

	topHeight := tuiMax(4, remaining/3)
	if remaining-topHeight < 5 {
		topHeight = tuiMax(3, remaining-5)
	}
	bottomHeight := tuiMax(4, remaining-topHeight)

	listContent := m.renderResultList()
	list := renderTUIViewport(listContent, width, topHeight, m.resultListOffset(topHeight), tuiPaneStyle)
	preview := renderTUIViewport(m.renderPreview(), width, bottomHeight, 0, tuiPaneStyle)
	return lipgloss.JoinVertical(lipgloss.Left, status, list, preview)
}

func (m tuiModel) renderResultsStatus(width int) string {
	var text string
	switch {
	case m.status != "":
		text = m.status
	case len(m.results) == 0:
		text = "No matches."
	default:
		text = fmt.Sprintf("Found %d sessions.", len(m.results))
	}
	var meta []string
	if m.scanned > 0 {
		meta = append(meta, fmt.Sprintf("scanned:%d", m.scanned))
	}
	if m.elapsed > 0 {
		meta = append(meta, m.elapsed.Round(time.Millisecond).String())
	}
	if len(m.warnings) > 0 {
		meta = append(meta, fmt.Sprintf("warnings:%d", len(m.warnings)))
	}
	meta = append(meta, "launch:"+m.launch.Label())
	meta = append(meta, "Enter open")
	meta = append(meta, "Tab launch")
	meta = append(meta, "c copy")
	meta = append(meta, "r cli")
	meta = append(meta, "q quit")
	if len(meta) > 0 {
		text += "  " + tuiDimStyle.Render(strings.Join(meta, " | "))
	}
	return lipgloss.NewStyle().Width(width).Render(text)
}

func (m tuiModel) renderUpdateFooter(width int) string {
	if m.updateNotice.Kind == updateNoticeNone {
		return ""
	}
	lines := updateNoticeLines(m.updateNotice, tuiMax(20, width-4), 4)
	if len(lines) == 0 {
		return ""
	}
	parts := []string{tuiDimStyle.Render("update")}
	for _, line := range lines {
		parts = append(parts, tuiDimStyle.Render(line))
	}
	return tuiPaneStyle.Width(width).Render(strings.Join(parts, "\n"))
}

func (m tuiModel) renderResultList() string {
	if len(m.results) == 0 {
		return tuiDimStyle.Render("No matching sessions.")
	}
	cfg := m.activeConfig()
	lines := make([]string, 0, len(m.results))
	for i, res := range m.results {
		title := nonEmpty(res.Title, "(untitled session)")
		title = shorten(title, cfg.Query, cfg.CaseSensitive, 90)
		title = highlightText(title, cfg.Query, cfg.CaseSensitive, outputTheme{Color: true})
		meta := tuiResultMetaLine(res)
		if meta != "" {
			meta = "  " + tuiDimStyle.Render(meta)
		}
		line := fmt.Sprintf("%3d  %s%s  %s", i+1, res.ID, meta, title)
		if i == m.selected {
			line = tuiSelectedStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func tuiResultMetaLine(res result) string {
	var parts []string
	if res.Date != "" {
		parts = append(parts, res.Date)
	}
	if res.CWD != "" {
		parts = append(parts, pathTail(res.CWD))
	}
	return strings.Join(parts, " | ")
}

func (m tuiModel) resultListOffset(height int) int {
	if len(m.results) == 0 {
		return 0
	}
	innerHeight := tuiMax(1, height-tuiPaneStyle.GetVerticalFrameSize())
	target := m.selected - innerHeight/2
	return tuiClamp(target, 0, tuiMax(0, len(m.results)-innerHeight))
}

func (m tuiModel) renderPreview() string {
	res, ok := m.selectedResult()
	if !ok {
		return tuiDimStyle.Render("No selected session.")
	}
	cfg := m.activeConfig()
	var lines []string
	lines = append(lines, tuiTitleStyle.Render(nonEmpty(res.Title, "(untitled session)")))
	lines = append(lines, "session: "+res.ID)
	lines = append(lines, "command: "+launchCommandString(res.ID, m.launch))
	if res.CWD != "" {
		lines = append(lines, "cwd: "+res.CWD)
	}
	lines = append(lines, "")

	if len(res.CommitMatches) > 0 {
		lines = append(lines, tuiCommitStyle.Render("commit matches"))
		for i, match := range res.CommitMatches {
			lines = append(lines, renderCommitPreviewLine(i, match, cfg))
		}
		lines = append(lines, "")
	}

	if len(res.Snippets) > 0 {
		if len(res.CommitMatches) > 0 {
			lines = append(lines, tuiStatusStyle.Render("session messages"))
		} else {
			lines = append(lines, tuiStatusStyle.Render("matches"))
		}
		for i, snip := range res.Snippets {
			if i > 0 {
				lines = append(lines, "")
			}
			if snip.Before != nil {
				lines = append(lines, renderMessagePreviewLine(*snip.Before, cfg, false))
			}
			lines = append(lines, renderMessagePreviewLine(snip.Match, cfg, true))
			if snip.After != nil {
				lines = append(lines, renderMessagePreviewLine(*snip.After, cfg, false))
			}
		}
	} else {
		lines = append(lines, tuiDimStyle.Render("No user or assistant preview is available for this session."))
	}
	return strings.Join(lines, "\n")
}

func (m tuiModel) activeConfig() config {
	cfg := config{
		Query:         m.activeTerm,
		Role:          "all",
		Snippets:      tuiDefaultSnippets,
		Limit:         tuiDefaultLimit,
		Root:          m.root,
		CaseSensitive: false,
	}
	if m.activeMode == tuiSearchCommit {
		cfg.CommitQuery = m.activeTerm
	}
	return cfg
}

func renderCommitPreviewLine(index int, match commitMatch, cfg config) string {
	parts := []string{fmt.Sprintf("%d.", index+1)}
	if match.Hash != "" {
		parts = append(parts, "hash:"+highlightCommitHash(match.Hash, cfg, outputTheme{Color: true}))
	}
	if match.FullHash != "" && match.FullHash != match.Hash {
		parts = append(parts, "full:"+highlightCommitHash(match.FullHash, cfg, outputTheme{Color: true}))
	}
	if match.Timestamp != "" {
		parts = append(parts, "time:"+trimTimestamp(match.Timestamp))
	}
	if match.CWD != "" {
		parts = append(parts, "cwd:"+match.CWD)
	}
	if match.Command != "" {
		parts = append(parts, "cmd:"+match.Command)
	}
	if match.Source != "" {
		parts = append(parts, "source:"+match.Source)
	}
	return strings.Join(parts, " | ")
}

func renderMessagePreviewLine(msg message, cfg config, matched bool) string {
	role := "[" + msg.Role + "]"
	roleStyle := tuiDimStyle
	switch msg.Role {
	case "user":
		roleStyle = tuiUserStyle
	case "assistant":
		roleStyle = tuiAssistantStyle
	}
	text := shorten(msg.Text, cfg.Query, cfg.CaseSensitive, 220)
	text = highlightText(text, cfg.Query, cfg.CaseSensitive, outputTheme{Color: true})
	if matched {
		return roleStyle.Render(role) + " " + text
	}
	return tuiDimStyle.Render(role) + " " + text
}

func renderTUIViewport(content string, width, height, yOffset int, style lipgloss.Style) string {
	width = tuiMax(1, width)
	height = tuiMax(1, height)
	innerWidth := tuiMax(1, width-style.GetHorizontalFrameSize())
	innerHeight := tuiMax(1, height-style.GetVerticalFrameSize())
	blockWidth := tuiMax(1, width-style.GetHorizontalBorderSize()-style.GetHorizontalMargins())
	blockHeight := tuiMax(1, height-style.GetVerticalBorderSize()-style.GetVerticalMargins())
	vp := viewport.New(innerWidth, innerHeight)
	vp.SetContent(content)
	if yOffset > 0 {
		vp.SetYOffset(yOffset)
	}
	return style.Width(blockWidth).Height(blockHeight).Render(vp.View())
}

func runTUISearch(root, query string, mode tuiSearchMode) ([]result, []string, int, error) {
	cfg := config{
		Query:    query,
		Root:     root,
		Limit:    tuiDefaultLimit,
		Snippets: tuiDefaultSnippets,
		Role:     "all",
		View:     defaultView,
	}
	if mode == tuiSearchCommit {
		normalized, err := normalizeCommitHashQuery(query)
		if err != nil {
			return nil, nil, 0, err
		}
		cfg.Query = normalized
		cfg.CommitQuery = normalized
	}

	manager, err := newIndexManager(root)
	if err != nil {
		return nil, nil, 0, err
	}

	var results []result
	var warnings []string
	var scanned int
	if cfg.CommitQuery != "" {
		results, warnings, scanned, err = searchCommitsWithIndex(manager, cfg)
	} else {
		results, warnings, scanned, err = searchWithIndex(manager, cfg)
		if err != nil {
			return runTUIRawSearch(cfg)
		}
	}
	if err != nil {
		return nil, nil, 0, err
	}
	sortResults(results)
	if cfg.Limit > 0 && len(results) > cfg.Limit {
		results = results[:cfg.Limit]
	}
	if cfg.CommitQuery != "" {
		addCommitSessionPreviews(manager, results)
	}
	return results, warnings, scanned, nil
}

func runTUIRawSearch(cfg config) ([]result, []string, int, error) {
	index, err := loadSessionIndex(filepath.Join(cfg.Root, "session_index.jsonl"))
	if err != nil {
		return nil, nil, 0, err
	}
	history, err := loadHistory(filepath.Join(cfg.Root, "history.jsonl"))
	if err != nil {
		return nil, nil, 0, err
	}
	from, to := effectiveDateRange(cfg)
	files, err := collectSessionFiles(filepath.Join(cfg.Root, "sessions"), from, to, cfg.LastSince, cfg.LastUntil)
	if err != nil {
		return nil, nil, 0, err
	}
	results, warnings := searchSessions(files, cfg, index, history)
	sortResults(results)
	if cfg.Limit > 0 && len(results) > cfg.Limit {
		results = results[:cfg.Limit]
	}
	return results, warnings, len(files), nil
}

func addCommitSessionPreviews(manager indexManager, results []result) {
	for i := range results {
		if len(results[i].Snippets) > 0 {
			continue
		}
		for _, msg := range loadIndexedPreviewMessages(manager, results[i], tuiPreviewMessages) {
			results[i].Snippets = append(results[i].Snippets, snippet{Match: msg})
		}
	}
}

func loadIndexedPreviewMessages(manager indexManager, res result, limit int) []message {
	if limit <= 0 || res.Path == "" {
		return nil
	}
	path := filepath.Join(manager.StorageDir, "sessions", indexFileName(res.Path))
	handle, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer handle.Close()

	var messages []message
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScannerLineBytes)
	for scanner.Scan() {
		var msg message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if !searchableRole(msg.Role, "all") {
			continue
		}
		messages = append(messages, msg)
		if len(messages) >= limit {
			break
		}
	}
	return messages
}

func normalizeTUIQuery(raw string, mode tuiSearchMode) (string, error) {
	query := strings.TrimSpace(raw)
	if query == "" {
		return "", errors.New("missing search query")
	}
	if mode == tuiSearchCommit {
		return normalizeCommitHashQuery(query)
	}
	return query, nil
}

func (mode tuiSearchMode) Label() string {
	if mode == tuiSearchCommit {
		return "commit hash"
	}
	return "normal"
}

func (mode tuiSearchMode) Placeholder() string {
	if mode == tuiSearchCommit {
		return "type a git commit hash..."
	}
	return "type keywords..."
}

func (mode tuiSearchMode) Toggle() tuiSearchMode {
	if mode == tuiSearchCommit {
		return tuiSearchText
	}
	return tuiSearchCommit
}

func (mode tuiLaunchMode) Label() string {
	if mode == tuiLaunchApp {
		return "app link"
	}
	return "cli"
}

func (mode tuiLaunchMode) Toggle() tuiLaunchMode {
	if mode == tuiLaunchApp {
		return tuiLaunchCLI
	}
	return tuiLaunchApp
}

func launchCommandString(sessionID string, mode tuiLaunchMode) string {
	if mode == tuiLaunchCLI {
		return "codex resume " + sessionID
	}
	cmd := deepLinkCommand(sessionID)
	return displayCommand(cmd.Path, cmd.Args[1:])
}

func deepLinkCommand(sessionID string) *exec.Cmd {
	url := "codex://threads/" + sessionID
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url)
	case "windows":
		return exec.Command("cmd", "/c", "start", "", url)
	default:
		return exec.Command("xdg-open", url)
	}
}

func displayCommand(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, filepath.Base(name))
	for _, arg := range args {
		parts = append(parts, quoteDisplayArg(arg))
	}
	return strings.Join(parts, " ")
}

func quoteDisplayArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if !strings.ContainsAny(arg, " \t\n'\"\\") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func writeClipboard(text string) error {
	cmd, err := clipboardCommand()
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(text)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return commandError(cmd, output, err)
	}
	return nil
}

func clipboardCommand() (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("pbcopy"), nil
	case "windows":
		return exec.Command("clip"), nil
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			return exec.Command("wl-copy"), nil
		}
		if _, err := exec.LookPath("xclip"); err == nil {
			return exec.Command("xclip", "-selection", "clipboard"), nil
		}
		if _, err := exec.LookPath("xsel"); err == nil {
			return exec.Command("xsel", "--clipboard", "--input"), nil
		}
		return nil, errors.New("no clipboard command found")
	}
}

func commandError(cmd *exec.Cmd, output []byte, err error) error {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return fmt.Errorf("%s: %w", displayCommand(cmd.Path, cmd.Args[1:]), err)
	}
	return fmt.Errorf("%s: %w: %s", displayCommand(cmd.Path, cmd.Args[1:]), err, normalizeWhitespace(text))
}

func tuiMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func tuiMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func tuiClamp(value, low, high int) int {
	if high < low {
		high = low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
