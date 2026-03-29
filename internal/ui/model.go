package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"nested-git-tui/internal/branchtree"
	"nested-git-tui/internal/ghpr"
	"nested-git-tui/internal/gitx"
	"nested-git-tui/internal/timefmt"
	"nested-git-tui/internal/tree"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type focus int

const (
	focusTree focus = iota
	focusPreview
)

type snapshot struct {
	current string
	unified []gitx.UnifiedBranch
	prs     []ghpr.PR
	gitErr  string
	ghErr   string
}

type snapshotMsg struct {
	path string
	snap snapshot
}

// fetchAllDoneMsg is sent after git fetch origin --prune has been run for every listed repo.
type fetchAllDoneMsg struct{}

// bulkSnapshotsMsg carries eager-loaded snapshots for many repos at once.
type bulkSnapshotsMsg struct {
	snaps map[string]snapshot
}

const preloadWorkers = 4

// Model is the interactive browser state.
type Model struct {
	scanRoot   string
	prLimit    int
	tr         *tree.Dir
	expanded   map[string]bool
	rows       []tree.Row
	cursor     int
	scroll     int
	treeInnerH int
	leftInnerW int
	leftColW   int
	leftFrac   float64 // fraction of total width for the left sidebar (Shift+←/→ adjusts)
	vp         viewport.Model
	focus      focus
	cache      map[string]snapshot
	allRepos   []string // every scanned repo path (for fetch + eager load)
	width      int
	height     int
}

// New builds the TUI model. absRepos must be non-empty absolute paths under scanRoot.
func New(scanRoot string, absRepos []string, prLimit int) *Model {
	scanRoot = filepath.Clean(scanRoot)
	tr := tree.Build(scanRoot, absRepos)
	m := &Model{
		scanRoot: scanRoot,
		prLimit:  prLimit,
		tr:       tr,
		expanded: map[string]bool{},
		focus:    focusTree,
		cache:    map[string]snapshot{},
		allRepos: append([]string(nil), absRepos...),
		// ~60% of the previous default (2/5): 0.4 * 0.6 = 0.24
		leftFrac: 0.24,
	}
	m.rebuildRows()

	m.vp = viewport.New(0, 0)
	m.vp.SetContent("Use ↑/↓ or j/k to move. Space expands/collapses folders. Tab switches panes.")

	return m
}

func (m *Model) rebuildRows() {
	var out []tree.Row
	tree.Flatten(m.tr, m.expanded, 0, &out)
	m.rows = out
	if len(m.rows) == 0 {
		m.cursor = 0
		m.scroll = 0
		return
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.ensureScroll()
}

func (m *Model) ensureScroll() {
	if m.treeInnerH <= 0 || len(m.rows) == 0 {
		return
	}
	maxScroll := len(m.rows) - m.treeInnerH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+m.treeInnerH {
		m.scroll = m.cursor - m.treeInnerH + 1
	}
}

func (m *Model) toggleFolder(rel string) {
	cur := tree.IsExpanded(m.expanded, rel)
	m.expanded[rel] = !cur
	target := rel
	m.rebuildRows()
	for i, r := range m.rows {
		if r.Kind == tree.RowFolder && r.Rel == target {
			m.cursor = i
			break
		}
	}
	m.ensureScroll()
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return fetchAllRepos(m.allRepos)
}

func (m *Model) handleTreeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.ensureScroll()
		}
		m.vp.SetContent(m.previewForSelection())
		return m, m.loadSelectedCmd()
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
			m.ensureScroll()
		}
		m.vp.SetContent(m.previewForSelection())
		return m, m.loadSelectedCmd()
	case "pgup":
		m.cursor -= m.treeInnerH
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.ensureScroll()
		m.vp.SetContent(m.previewForSelection())
		return m, m.loadSelectedCmd()
	case "pgdown":
		m.cursor += m.treeInnerH
		if m.cursor >= len(m.rows) {
			m.cursor = len(m.rows) - 1
		}
		m.ensureScroll()
		m.vp.SetContent(m.previewForSelection())
		return m, m.loadSelectedCmd()
	case "home", "g":
		m.cursor = 0
		m.scroll = 0
		m.vp.SetContent(m.previewForSelection())
		return m, m.loadSelectedCmd()
	case "end", "G":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
			m.ensureScroll()
		}
		m.vp.SetContent(m.previewForSelection())
		return m, m.loadSelectedCmd()
	case " ":
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			r := m.rows[m.cursor]
			if r.Kind == tree.RowFolder {
				m.toggleFolder(r.Rel)
				m.vp.SetContent(m.previewForSelection())
				return m, m.loadSelectedCmd()
			}
		}
		return m, nil
	}
	return m, nil
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case snapshotMsg:
		m.cache[msg.path] = msg.snap
		if m.selectedAbs() == msg.path {
			m.vp.SetContent(m.renderPreview(msg.path))
			m.vp.GotoTop()
		}
		return m, nil

	case fetchAllDoneMsg:
		return m, preloadAllSnapshots(m.allRepos, m.prLimit)

	case bulkSnapshotsMsg:
		for p, snap := range msg.snaps {
			m.cache[p] = snap
		}
		m.vp.SetContent(m.previewForSelection())
		m.vp.GotoTop()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "shift+left":
			m.leftFrac -= 0.02
			if m.leftFrac < 0.12 {
				m.leftFrac = 0.12
			}
			m.layout()
			return m, nil
		case "shift+right":
			m.leftFrac += 0.02
			if m.leftFrac > 0.52 {
				m.leftFrac = 0.52
			}
			m.layout()
			return m, nil
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab":
			if m.focus == focusTree {
				m.focus = focusPreview
			} else {
				m.focus = focusTree
			}
			return m, nil
		case "r":
			p := m.selectedAbs()
			if p == "" {
				return m, nil
			}
			delete(m.cache, p)
			m.vp.SetContent("Refreshing…")
			return m, refreshRepoSnapshot(p, m.prLimit)
		}

		if m.focus == focusPreview {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		return m.handleTreeKey(msg)
	}

	if m.focus == focusPreview {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// Reserve an extra line so the leading newline in View() does not push the bottom off-screen.
	contentH := m.height - 3
	if contentH < 6 {
		contentH = 6
	}
	leftW := int(float64(m.width) * m.leftFrac)
	if leftW < 22 {
		leftW = 22
	}
	if leftW > m.width-20 {
		leftW = m.width - 20
	}
	rightW := m.width - leftW - 2
	if rightW < 20 {
		rightW = m.width / 2
		leftW = m.width - rightW - 2
	}
	m.treeInnerH = contentH - 2
	if m.treeInnerH < 3 {
		m.treeInnerH = 3
	}
	m.leftInnerW = leftW - 4
	if m.leftInnerW < 12 {
		m.leftInnerW = 12
	}
	m.leftColW = leftW
	m.vp.Width = rightW
	m.vp.Height = contentH
	m.vp.SetContent(m.previewForSelection())
	m.ensureScroll()
}

func (m *Model) selectedAbs() string {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return ""
	}
	r := m.rows[m.cursor]
	if r.Kind != tree.RowRepo {
		return ""
	}
	return r.Abs
}

func (m *Model) previewForSelection() string {
	p := m.selectedAbs()
	if p == "" {
		if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].Kind == tree.RowFolder {
			return fmt.Sprintf("%s\n\n(folder — press Space to expand/collapse; select a repo for details)",
				lipgloss.NewStyle().Bold(true).Render(m.rows[m.cursor].Label))
		}
		return "Select a repository in the tree (expand folders with Space)."
	}
	if snap, ok := m.cache[p]; ok {
		return m.renderSnapshot(p, snap)
	}
	return "Loading…"
}

func (m *Model) renderPreview(path string) string {
	snap, ok := m.cache[path]
	if !ok {
		return "Loading…"
	}
	return m.renderSnapshot(path, snap)
}

func (m *Model) relDisplay(abs string) string {
	rel, err := filepath.Rel(m.scanRoot, abs)
	if err != nil {
		return abs
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." {
		return filepath.Base(m.scanRoot)
	}
	return rel
}

func (m *Model) renderSnapshot(path string, s snapshot) string {
	now := time.Now()
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(m.relDisplay(path)))
	b.WriteString("\n\n")
	if s.current != "" {
		fmt.Fprintf(&b, "Checked out: %s\n\n", s.current)
	} else {
		b.WriteString("Checked out: (detached or empty)\n\n")
	}

	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Branches & open PRs"))
	b.WriteString("\n")
	webBase, _ := gitx.GitHubWebBase(path)
	b.WriteString(renderBranchTree(path, s, now, webBase))
	if s.ghErr != "" {
		fmt.Fprintf(&b, "\n  %s\n", s.ghErr)
		if hint := ghpr.AuthHint(); hint != "" {
			fmt.Fprintf(&b, "  %s\n", hint)
		}
	}
	return b.String()
}

func renderBranchTree(repoPath string, s snapshot, now time.Time, webBase string) string {
	if s.gitErr != "" {
		return fmt.Sprintf("  error: %s\n", s.gitErr)
	}
	if !gitx.IsWorkTree(repoPath) {
		return "  (not a normal work tree)\n"
	}
	if len(s.unified) == 0 && len(s.prs) == 0 {
		return "  (no branches)\n"
	}
	root := branchtree.Build(s.unified)
	var prs []ghpr.PR
	if s.ghErr == "" {
		prs = s.prs
	}
	unmatched := branchtree.AssignPRs(root, prs)
	var rows []branchtree.Row
	branchtree.Flatten(root, 0, &rows)
	var out strings.Builder
	for _, r := range rows {
		ind := strings.Repeat("  ", r.Depth)
		switch r.Kind {
		case branchtree.RowFolder:
			fmt.Fprintf(&out, "%s▾ %s\n", ind, r.Label)
		case branchtree.RowBranch:
			if r.U == nil {
				continue
			}
			label := r.Label
			if u := gitx.BranchTreeURL(webBase, r.U.FullName); u != "" {
				label = OSC8(u, label)
			}
			fmt.Fprintf(&out, "%s◦ %s  %s\n", ind, label, formatUnifiedBranchMeta(r.U, now))
		case branchtree.RowPR:
			if r.PR == nil {
				continue
			}
			pr := r.PR
			opened := pr.CreatedAt
			if t, err := timefmt.ParseGitHub(pr.CreatedAt); err == nil {
				opened = timefmt.Relative(t, now)
			}
			title := pr.Title
			if pr.URL != "" {
				title = OSC8(pr.URL, title)
			}
			head := fmt.Sprintf("#%d", pr.Number)
			if pr.URL != "" {
				head = OSC8(pr.URL, head)
			}
			fmt.Fprintf(&out, "%s  %s %s  %s  %s\n", ind, head, title, ghpr.FormatPRTagLine(*pr), lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(opened))
		}
	}
	if len(unmatched) > 0 {
		fmt.Fprintf(&out, "▸ PRs (no matching branch)\n")
		for i := range unmatched {
			pr := &unmatched[i]
			opened := pr.CreatedAt
			if t, err := timefmt.ParseGitHub(pr.CreatedAt); err == nil {
				opened = timefmt.Relative(t, now)
			}
			title := pr.Title
			if pr.URL != "" {
				title = OSC8(pr.URL, title)
			}
			head := fmt.Sprintf("#%d", pr.Number)
			if pr.URL != "" {
				head = OSC8(pr.URL, head)
			}
			fmt.Fprintf(&out, "  %s %s  %s  %s\n", head, title, ghpr.FormatPRTagLine(*pr), lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(opened))
		}
	}
	return out.String()
}

func formatUnifiedBranchMeta(u *gitx.UnifiedBranch, now time.Time) string {
	var chips []string
	if u.Local != nil {
		chips = append(chips, lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("114")).Padding(0, 1).Render("local"))
	}
	if u.Remote != nil {
		chips = append(chips, lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("117")).Padding(0, 1).Render("remote"))
	}
	chipStr := strings.Join(chips, " ")
	if chipStr == "" {
		chipStr = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("—")
	}

	sync := ""
	switch {
	case u.Local != nil && u.Remote != nil:
		if u.SyncErr != "" {
			sync = "  ⚠ " + truncateRunes(u.SyncErr, 48)
		} else if u.Ahead >= 0 && u.Behind >= 0 {
			sync = fmt.Sprintf("  %d↑ %d↓", u.Ahead, u.Behind)
		}
	}

	when := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(branchTipWhen(u, now))
	return strings.TrimRight(strings.Join([]string{chipStr, sync, "  ", when}, ""), " ")
}

func branchTipWhen(u *gitx.UnifiedBranch, now time.Time) string {
	var t time.Time
	if u.Local != nil && !u.Local.When.IsZero() {
		t = u.Local.When
	}
	if u.Remote != nil && !u.Remote.When.IsZero() {
		if t.IsZero() || u.Remote.When.After(t) {
			t = u.Remote.When
		}
	}
	if t.IsZero() {
		return "?"
	}
	return timefmt.Relative(t, now)
}

func (m *Model) loadSelectedCmd() tea.Cmd {
	p := m.selectedAbs()
	if p == "" {
		return nil
	}
	if _, ok := m.cache[p]; ok {
		return nil
	}
	return loadSnapshot(p, m.prLimit)
}

func loadSnapshot(path string, prLimit int) tea.Cmd {
	return func() tea.Msg {
		return snapshotMsg{path: path, snap: buildSnapshot(path, prLimit)}
	}
}

// buildSnapshot loads branch + PR data for one repo (used by eager load and refresh).
func buildSnapshot(path string, prLimit int) snapshot {
	s := snapshot{current: gitx.CurrentBranch(path)}
	if gitx.IsWorkTree(path) {
		ub, err := gitx.CollectUnifiedBranches(path)
		if err != nil {
			s.gitErr = err.Error()
		} else {
			s.unified = ub
		}
	}
	if err := ghpr.RepoViewable(path); err == nil {
		prs, err := ghpr.ListOpen(path, prLimit)
		if err != nil {
			s.ghErr = err.Error()
			if hint := ghpr.AuthHint(); hint != "" {
				s.ghErr = s.ghErr + "\n" + hint
			}
		} else {
			s.prs = prs
		}
	} else {
		s.ghErr = fmt.Sprintf("GitHub: %v", err)
		if hint := ghpr.AuthHint(); hint != "" {
			s.ghErr = s.ghErr + "\n" + hint
		}
	}
	return s
}

// fetchAllRepos runs git fetch origin --prune in every repo (parallel, bounded).
func fetchAllRepos(repos []string) tea.Cmd {
	return func() tea.Msg {
		if len(repos) == 0 {
			return fetchAllDoneMsg{}
		}
		sem := make(chan struct{}, 4)
		var wg sync.WaitGroup
		for _, p := range repos {
			wg.Add(1)
			go func(repo string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				_ = gitx.FetchOriginPrune(repo)
			}(p)
		}
		wg.Wait()
		return fetchAllDoneMsg{}
	}
}

// preloadAllSnapshots builds snapshots for all repos with bounded parallelism.
func preloadAllSnapshots(repos []string, prLimit int) tea.Cmd {
	return func() tea.Msg {
		if len(repos) == 0 {
			return bulkSnapshotsMsg{snaps: nil}
		}
		sem := make(chan struct{}, preloadWorkers)
		var wg sync.WaitGroup
		var mu sync.Mutex
		out := make(map[string]snapshot, len(repos))
		for _, p := range repos {
			wg.Add(1)
			go func(repo string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				snap := buildSnapshot(repo, prLimit)
				mu.Lock()
				out[repo] = snap
				mu.Unlock()
			}(p)
		}
		wg.Wait()
		return bulkSnapshotsMsg{snaps: out}
	}
}

// refreshRepoSnapshot re-fetches origin for one repo then reloads its snapshot.
func refreshRepoSnapshot(path string, prLimit int) tea.Cmd {
	return func() tea.Msg {
		_ = gitx.FetchOriginPrune(path)
		return snapshotMsg{path: path, snap: buildSnapshot(path, prLimit)}
	}
}

func (m *Model) renderTreePanel() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205")).Render("Repositories")
	sub := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("Space: expand/collapse · j/k: move")
	header := lipgloss.JoinVertical(lipgloss.Left, title, sub)

	var lines []string
	if len(m.rows) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("(no repos)"))
	} else {
		end := m.scroll + m.treeInnerH
		if end > len(m.rows) {
			end = len(m.rows)
		}
		for i := m.scroll; i < end; i++ {
			lines = append(lines, m.formatTreeLine(m.rows[i], i == m.cursor))
		}
	}
	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func (m *Model) formatTreeLine(row tree.Row, selected bool) string {
	indent := strings.Repeat("  ", row.Depth)
	var sym string
	switch row.Kind {
	case tree.RowFolder:
		if tree.IsExpanded(m.expanded, row.Rel) {
			sym = "▾ "
		} else {
			sym = "▸ "
		}
	default:
		sym = "◦ "
	}
	raw := indent + sym + row.Label
	raw = truncateRunes(raw, m.leftInnerW)
	st := lipgloss.NewStyle()
	if selected {
		st = st.Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252"))
	}
	return st.Render(raw)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(runes[:max-1]) + "…"
}

// View implements tea.Model.
func (m *Model) View() string {
	if m.width == 0 {
		return "Initializing…"
	}
	left := m.renderTreePanel()
	border := lipgloss.RoundedBorder()
	leftBorder := lipgloss.Color("240")
	if m.focus == focusTree {
		leftBorder = lipgloss.Color("205")
	}
	leftPane := lipgloss.NewStyle().
		Border(border).
		BorderForeground(leftBorder).
		Width(m.leftColW).
		Height(m.vp.Height + 2).
		Render(left)

	rightBody := m.vp.View()
	rightPane := lipgloss.NewStyle().
		Border(border).
		BorderForeground(func() lipgloss.Color {
			if m.focus == focusPreview {
				return lipgloss.Color("205")
			}
			return lipgloss.Color("240")
		}()).
		Width(m.vp.Width + 2).
		Height(m.vp.Height + 2).
		Render(rightBody)

	row := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, "  ", rightPane)

	help := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Render("tab · shift+←/→ · space · g/G · r: fetch+reload repo · q · (startup: fetch --prune + preload all)")

	// Leading newline avoids the first row sitting under the terminal/IDE chrome in some hosts.
	return "\n" + lipgloss.JoinVertical(lipgloss.Left, row, "", help)
}
