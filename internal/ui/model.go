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

	"github.com/charmbracelet/bubbles/key"
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

type archiveDoneMsg struct {
	repo        string
	branch      string
	err         error
	pendingLine int
	pendingOk   bool
}

type checkoutDoneMsg struct {
	repo string
	err  error
}

// archiveConfirmDialog blocks the UI until the user confirms or cancels archive.
type archiveConfirmDialog struct {
	repo         string
	branch       string
	dontAskAgain bool
}

// deleteConfirmDialog blocks the UI until the user confirms or cancels branch delete.
type deleteConfirmDialog struct {
	repo          string
	branch        string
	dontAskAgain  bool
	switchToFirst string // if non-empty, branch is checked out; confirm checks this out before delete
}

type deleteDoneMsg struct {
	repo        string
	branch      string
	err         error
	pendingLine int
	pendingOk   bool
}

type pushPRDoneMsg struct {
	repo   string
	branch string
	err    error
}

const preloadWorkers = 4

// Model is the interactive browser state.
type Model struct {
	scanRoot           string
	prLimit            int
	tr                 *tree.Dir
	expanded           map[string]bool
	rows               []tree.Row
	cursor             int
	scroll             int
	treeInnerH         int
	leftInnerW         int
	leftColW           int
	leftFrac           float64 // fraction of total width for the left sidebar (Shift+←/→ adjusts)
	vp                 viewport.Model
	previewSelLine     int
	lastPreviewRowKey  string // tree.RowKind + Rel — reset line when selection changes
	focus              focus
	cache              map[string]snapshot
	branchExpanded     map[string]bool     // key: repoAbs + "\x00" + folder Rel — preview branch tree folders
	allRepos           []string            // every scanned repo path (for fetch + eager load)
	worktreesByRepo    map[string][]string // primary abs -> all linked worktree paths (primary first); nil if unknown
	prefs              Prefs
	archiveConfirm     *archiveConfirmDialog
	deleteConfirm      *deleteConfirmDialog
	statusMsg          string
	width              int
	height             int
	pendingPreviewRepo string // repo path; cleared after snapshotMsg applies pending line
	pendingPreviewLine int    // absolute preview line; -1 means none
	previewPaneH       int    // last laid-out inner height for the right column (header + viewport)
	previewHeader      string // fixed region above the scrollable branch list (styled); empty if not split
}

// New builds the TUI model. absRepos must be non-empty absolute paths under scanRoot (one per logical repo).
// worktreesByPrimary maps each primary path to all linked worktree paths including the primary; nil is allowed.
func New(scanRoot string, absRepos []string, worktreesByPrimary map[string][]string, prLimit int) *Model {
	scanRoot = filepath.Clean(scanRoot)
	tr := tree.Build(scanRoot, absRepos)
	m := &Model{
		scanRoot:        scanRoot,
		prLimit:         prLimit,
		tr:              tr,
		expanded:        map[string]bool{},
		focus:           focusTree,
		cache:           map[string]snapshot{},
		allRepos:        append([]string(nil), absRepos...),
		worktreesByRepo: worktreesByPrimary,
		// ~60% of the previous default (2/5): 0.4 * 0.6 = 0.24
		leftFrac: 0.24,
		prefs:    loadPrefs(),
	}
	m.rebuildRows()

	m.vp = viewport.New(0, 0)
	km := m.vp.KeyMap
	km.PageDown = key.NewBinding(
		key.WithKeys("pgdown", "f"),
		key.WithHelp("f/pgdn", "page down"),
	)
	m.vp.KeyMap = km
	m.lastPreviewRowKey = m.currentRowKey()
	m.pendingPreviewLine = -1

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
		m.resetPreviewLineForNewTreeRow()
		m.applyPreviewContent()
		return m, m.loadSelectedCmd()
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
			m.ensureScroll()
		}
		m.resetPreviewLineForNewTreeRow()
		m.applyPreviewContent()
		return m, m.loadSelectedCmd()
	case "pgup":
		m.cursor -= m.treeInnerH
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.ensureScroll()
		m.resetPreviewLineForNewTreeRow()
		m.applyPreviewContent()
		return m, m.loadSelectedCmd()
	case "pgdown":
		m.cursor += m.treeInnerH
		if m.cursor >= len(m.rows) {
			m.cursor = len(m.rows) - 1
		}
		m.ensureScroll()
		m.resetPreviewLineForNewTreeRow()
		m.applyPreviewContent()
		return m, m.loadSelectedCmd()
	case "home", "g":
		m.cursor = 0
		m.scroll = 0
		m.resetPreviewLineForNewTreeRow()
		m.applyPreviewContent()
		return m, m.loadSelectedCmd()
	case "end", "G":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
			m.ensureScroll()
		}
		m.resetPreviewLineForNewTreeRow()
		m.applyPreviewContent()
		return m, m.loadSelectedCmd()
	case " ":
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			r := m.rows[m.cursor]
			if r.Kind == tree.RowFolder {
				m.toggleFolder(r.Rel)
				m.resetPreviewLineForNewTreeRow()
				m.applyPreviewContent()
				return m, m.loadSelectedCmd()
			}
		}
		return m, nil
	case "enter":
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			r := m.rows[m.cursor]
			if r.Kind == tree.RowRepo {
				u := gitx.RepoBranchesAllURL(r.Abs)
				if u == "" {
					m.statusMsg = "GitHub branches page not available (need github.com origin)."
					return m, nil
				}
				return m, openURLCmd(u)
			}
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) handlePreviewKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.previewSelLine > 0 {
			m.previewSelLine--
		}
		m.applyPreviewContent()
		return m, nil
	case "down", "j":
		n := m.previewPlainLineCount()
		if n > 0 && m.previewSelLine < n-1 {
			m.previewSelLine++
		}
		m.applyPreviewContent()
		return m, nil
	case "pgup":
		m.previewSelLine -= m.vp.Height
		if m.previewSelLine < 0 {
			m.previewSelLine = 0
		}
		m.applyPreviewContent()
		return m, nil
	case "pgdown":
		n := m.previewPlainLineCount()
		if n == 0 {
			return m, nil
		}
		m.previewSelLine += m.vp.Height
		if m.previewSelLine >= n {
			m.previewSelLine = n - 1
		}
		m.applyPreviewContent()
		return m, nil
	case "home", "g":
		m.previewSelLine = 0
		m.applyPreviewContent()
		return m, nil
	case "end", "G":
		n := m.previewPlainLineCount()
		if n > 0 {
			m.previewSelLine = n - 1
		}
		m.applyPreviewContent()
		return m, nil
	case "enter":
		u := m.previewURLAtSelLine()
		if u == "" {
			return m, nil
		}
		return m, openURLCmd(u)
	case " ":
		repo, rel, ok := m.branchFolderAtPreviewLine()
		if !ok {
			return m, nil
		}
		m.toggleBranchFolder(repo, rel)
		m.applyPreviewContent()
		return m, nil
	case "c", "C":
		return m.handleCheckoutRequest()
	case "d", "D":
		return m.handleDeleteBranchRequest()
	case "p", "P":
		return m.handlePushPRRequest()
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *Model) previewPlainLineCount() int {
	plain := strings.ReplaceAll(m.previewForSelection(), "\r\n", "\n")
	if plain == "" {
		return 0
	}
	return len(strings.Split(plain, "\n"))
}

func (m *Model) previewURLAtSelLine() string {
	p := m.selectedAbs()
	if p == "" {
		return ""
	}
	snap, ok := m.cache[p]
	if !ok {
		return ""
	}
	start := m.branchTreeContentStartLine(p, snap)
	idx := m.previewSelLine - start
	if idx < 0 {
		return ""
	}
	webBase, _ := gitx.GitHubWebBase(p)
	urls := m.previewBranchLineURLs(p, snap, webBase)
	if idx >= len(urls) {
		return ""
	}
	return urls[idx]
}

// previewBranchLineURLs returns one URL per rendered line in renderBranchTree (same order, including skips).
func (m *Model) previewBranchLineURLs(repoPath string, s snapshot, webBase string) []string {
	if s.gitErr != "" {
		return []string{""}
	}
	if !gitx.IsWorkTree(repoPath) {
		return []string{""}
	}
	if len(s.unified) == 0 && len(s.prs) == 0 {
		return []string{""}
	}
	rows, unmatched := m.branchRowsAndUnmatched(repoPath, s)
	var urls []string
	for _, r := range rows {
		switch r.Kind {
		case branchtree.RowFolder:
			urls = append(urls, "")
		case branchtree.RowBranch:
			if r.U == nil {
				continue
			}
			urls = append(urls, gitx.BranchTreeURL(webBase, r.U.FullName))
		case branchtree.RowPR:
			if r.PR == nil {
				continue
			}
			urls = append(urls, r.PR.URL)
		}
	}
	if len(unmatched) > 0 {
		urls = append(urls, "")
		for i := range unmatched {
			urls = append(urls, unmatched[i].URL)
		}
	}
	return urls
}

func branchFolderKey(repoAbs, folderRel string) string {
	return repoAbs + "\x00" + folderRel
}

func (m *Model) isBranchFolderExpanded(repoAbs, folderRel string) bool {
	if m.branchExpanded == nil {
		return true
	}
	v, ok := m.branchExpanded[branchFolderKey(repoAbs, folderRel)]
	if !ok {
		return true
	}
	return v
}

func (m *Model) toggleBranchFolder(repoAbs, folderRel string) {
	if m.branchExpanded == nil {
		m.branchExpanded = map[string]bool{}
	}
	k := branchFolderKey(repoAbs, folderRel)
	m.branchExpanded[k] = !m.isBranchFolderExpanded(repoAbs, folderRel)
}

func (m *Model) snapshotHeaderBeforeTree(path string, s snapshot) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Render(m.relDisplay(path)))
	b.WriteString("\n\n")
	if s.current != "" {
		fmt.Fprintf(&b, "Checked out: %s\n\n", s.current)
	} else {
		b.WriteString("Checked out: (detached or empty)\n\n")
	}
	wts := m.worktreesForRepo(path)
	if len(wts) > 1 {
		b.WriteString(lipgloss.NewStyle().Bold(true).Render("Worktrees"))
		b.WriteString("\n")
		for _, wt := range wts {
			label := m.relDisplay(wt)
			suffix := ""
			if filepath.Clean(wt) == filepath.Clean(path) {
				suffix = "  (primary)"
			}
			fmt.Fprintf(&b, "  ◦ %s%s\n", label, suffix)
		}
		b.WriteString("\n")
	}
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Branches & open PRs"))
	b.WriteString("\n")
	return b.String()
}

func (m *Model) worktreesForRepo(primary string) []string {
	if m.worktreesByRepo == nil {
		return nil
	}
	wts := m.worktreesByRepo[primary]
	if len(wts) == 0 {
		return nil
	}
	return wts
}

func firstLineIndexAfterHeader(header string) int {
	h := strings.TrimSuffix(header, "\n")
	if h == "" {
		return 0
	}
	return len(strings.Split(h, "\n"))
}

func (m *Model) branchTreeContentStartLine(path string, s snapshot) int {
	return firstLineIndexAfterHeader(m.snapshotHeaderBeforeTree(path, s))
}

func nextPreviewLineAfterRemoval(deletedIdx, oldCount int) int {
	if oldCount <= 1 {
		return 0
	}
	if deletedIdx < oldCount-1 {
		return deletedIdx
	}
	return deletedIdx - 1
}

func stringLineCount(s string) int {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// capturePendingPreviewAfterBranchRemoval returns the preview line to select after removing one
// branch-tree row (same visual index semantics as renderBranchTree output).
func (m *Model) capturePendingPreviewAfterBranchRemoval(repo string) (pendingLine int, pendingOk bool) {
	p := m.selectedAbs()
	if p != repo {
		return 0, false
	}
	snap, ok := m.cache[p]
	if !ok {
		return 0, false
	}
	treeStart := m.branchTreeContentStartLine(p, snap)
	webBase, _ := gitx.GitHubWebBase(p)
	branchBlob := m.renderBranchTree(p, snap, time.Now(), webBase)
	visualLines := stringLineCount(branchBlob)
	if visualLines == 0 {
		return 0, false
	}
	deletedIdx := m.previewSelLine - treeStart
	if deletedIdx < 0 || deletedIdx >= visualLines {
		return 0, false
	}
	newIdx := nextPreviewLineAfterRemoval(deletedIdx, visualLines)
	return treeStart + newIdx, true
}

func (m *Model) branchRowsAndUnmatched(repoPath string, s snapshot) (rows []branchtree.Row, unmatched []ghpr.PR) {
	if s.gitErr != "" {
		return nil, nil
	}
	if !gitx.IsWorkTree(repoPath) {
		return nil, nil
	}
	if len(s.unified) == 0 && len(s.prs) == 0 {
		return nil, nil
	}
	root := branchtree.Build(s.unified)
	var prs []ghpr.PR
	if s.ghErr == "" {
		prs = s.prs
	}
	unmatched = branchtree.AssignPRs(root, prs)
	branchtree.Flatten(root, 0, func(rel string) bool {
		return m.isBranchFolderExpanded(repoPath, rel)
	}, &rows)
	return rows, unmatched
}

func (m *Model) branchFolderAtPreviewLine() (repoAbs, folderRel string, ok bool) {
	p := m.selectedAbs()
	if p == "" {
		return "", "", false
	}
	snap, ok := m.cache[p]
	if !ok {
		return "", "", false
	}
	start := m.branchTreeContentStartLine(p, snap)
	if m.previewSelLine < start {
		return "", "", false
	}
	rows, _ := m.branchRowsAndUnmatched(p, snap)
	if len(rows) == 0 {
		return "", "", false
	}
	idx := m.previewSelLine - start
	if idx < 0 || idx >= len(rows) {
		return "", "", false
	}
	r := rows[idx]
	if r.Kind != branchtree.RowFolder {
		return "", "", false
	}
	return p, r.Rel, true
}

// selectedPreviewBranchName returns the repo and branch to operate on when the preview
// highlight is on a branch row or a PR row (head branch).
func (m *Model) selectedPreviewBranchName() (repoAbs, branch string, ok bool) {
	p := m.selectedAbs()
	if p == "" {
		return "", "", false
	}
	snap, have := m.cache[p]
	if !have {
		return "", "", false
	}
	start := m.branchTreeContentStartLine(p, snap)
	rows, _ := m.branchRowsAndUnmatched(p, snap)
	if len(rows) == 0 {
		return "", "", false
	}
	idx := m.previewSelLine - start
	if idx < 0 || idx >= len(rows) {
		return "", "", false
	}
	r := rows[idx]
	switch r.Kind {
	case branchtree.RowBranch:
		if r.U == nil {
			return "", "", false
		}
		return p, r.U.FullName, true
	case branchtree.RowPR:
		if r.PR == nil {
			return "", "", false
		}
		return p, ghpr.HeadBranchLocalName(r.PR.HeadRefName), true
	default:
		return "", "", false
	}
}

func (m *Model) handleArchiveBranchRequest() (tea.Model, tea.Cmd) {
	repo, branch, ok := m.selectedPreviewBranchName()
	if !ok {
		return m, nil
	}
	m.statusMsg = ""
	pl, pok := m.capturePendingPreviewAfterBranchRemoval(repo)
	if m.prefs.SkipArchiveConfirm {
		return m, m.runArchiveBranchCmd(repo, branch, pl, pok)
	}
	m.archiveConfirm = &archiveConfirmDialog{repo: repo, branch: branch}
	return m, nil
}

func (m *Model) handleArchiveDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.archiveConfirm == nil {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case " ":
		m.archiveConfirm.dontAskAgain = !m.archiveConfirm.dontAskAgain
		return m, nil
	case "y", "Y", "enter":
		d := m.archiveConfirm
		m.archiveConfirm = nil
		pl, pok := m.capturePendingPreviewAfterBranchRemoval(d.repo)
		if d.dontAskAgain {
			m.prefs.SkipArchiveConfirm = true
			_ = savePrefs(m.prefs)
		}
		return m, m.runArchiveBranchCmd(d.repo, d.branch, pl, pok)
	case "n", "N", "esc", "q":
		m.archiveConfirm = nil
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) runArchiveBranchCmd(repo, branch string, pendingLine int, pendingOk bool) tea.Cmd {
	return func() tea.Msg {
		err := gitx.ArchiveBranch(repo, branch)
		return archiveDoneMsg{repo: repo, branch: branch, err: err, pendingLine: pendingLine, pendingOk: pendingOk}
	}
}

func (m *Model) handleDeleteBranchRequest() (tea.Model, tea.Cmd) {
	repo, branch, ok := m.selectedPreviewBranchName()
	if !ok {
		return m, nil
	}
	m.statusMsg = ""
	switchTo := ""
	if gitx.CurrentBranch(repo) == branch {
		t, err := gitx.DefaultBranchToCheckout(repo, branch)
		if err != nil {
			m.statusMsg = fmt.Sprintf("Cannot delete: %v", err)
			return m, nil
		}
		switchTo = t
	}
	pl, pok := m.capturePendingPreviewAfterBranchRemoval(repo)
	if m.prefs.SkipDeleteConfirm {
		return m, m.runDeleteBranchCmd(repo, branch, switchTo, pl, pok)
	}
	m.deleteConfirm = &deleteConfirmDialog{repo: repo, branch: branch, switchToFirst: switchTo}
	return m, nil
}

func (m *Model) handleDeleteDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.deleteConfirm == nil {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case " ":
		m.deleteConfirm.dontAskAgain = !m.deleteConfirm.dontAskAgain
		return m, nil
	case "y", "Y", "enter":
		d := m.deleteConfirm
		m.deleteConfirm = nil
		pl, pok := m.capturePendingPreviewAfterBranchRemoval(d.repo)
		if d.dontAskAgain {
			m.prefs.SkipDeleteConfirm = true
			_ = savePrefs(m.prefs)
		}
		return m, m.runDeleteBranchCmd(d.repo, d.branch, d.switchToFirst, pl, pok)
	case "n", "N", "esc", "q":
		m.deleteConfirm = nil
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) runDeleteBranchCmd(repo, branch, switchToFirst string, pendingLine int, pendingOk bool) tea.Cmd {
	return func() tea.Msg {
		err := gitx.DeleteBranch(repo, branch, switchToFirst)
		return deleteDoneMsg{repo: repo, branch: branch, err: err, pendingLine: pendingLine, pendingOk: pendingOk}
	}
}

func (m *Model) handleCheckoutRequest() (tea.Model, tea.Cmd) {
	repo, branch, ok := m.selectedPreviewBranchName()
	if !ok {
		return m, nil
	}
	m.statusMsg = ""
	return m, m.runCheckoutCmd(repo, branch)
}

func (m *Model) runCheckoutCmd(repo, branch string) tea.Cmd {
	return func() tea.Msg {
		err := gitx.Checkout(repo, branch)
		return checkoutDoneMsg{repo: repo, err: err}
	}
}

func (m *Model) handlePushPRRequest() (tea.Model, tea.Cmd) {
	repo, branch, ok := m.selectedPreviewBranchName()
	if !ok {
		return m, nil
	}
	m.statusMsg = ""
	return m, m.runPushPRCmd(repo, branch)
}

func (m *Model) runPushPRCmd(repo, branch string) tea.Cmd {
	return func() tea.Msg {
		err := gitx.PushCreatePR(repo, branch)
		return pushPRDoneMsg{repo: repo, branch: branch, err: err}
	}
}

func (m *Model) linkText(url, plain string) string {
	if m.prefs.ShowPreviewLinks && url != "" {
		return OSC8(url, plain)
	}
	return plain
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
			if m.pendingPreviewRepo == msg.path && m.pendingPreviewLine >= 0 {
				m.previewSelLine = m.pendingPreviewLine
				m.pendingPreviewRepo = ""
				m.pendingPreviewLine = -1
			} else {
				m.previewSelLine = 0
			}
			m.applyPreviewContent()
		}
		return m, nil

	case fetchAllDoneMsg:
		return m, preloadAllSnapshots(m.allRepos, m.prLimit)

	case bulkSnapshotsMsg:
		for p, snap := range msg.snaps {
			m.cache[p] = snap
		}
		m.previewSelLine = 0
		m.applyPreviewContent()
		return m, nil

	case archiveDoneMsg:
		// Ensure modal is gone after async work (covers any missed confirm-path clear).
		m.archiveConfirm = nil
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Archive failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Archived %s", msg.branch)
			if msg.pendingOk {
				m.pendingPreviewRepo = msg.repo
				m.pendingPreviewLine = msg.pendingLine
			}
		}
		delete(m.cache, msg.repo)
		return m, loadSnapshot(msg.repo, m.prLimit)

	case checkoutDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Checkout failed: %v", msg.err)
		} else {
			m.statusMsg = "Checked out."
		}
		delete(m.cache, msg.repo)
		return m, loadSnapshot(msg.repo, m.prLimit)

	case deleteDoneMsg:
		// Ensure modal is gone after async work (covers any missed confirm-path clear).
		m.deleteConfirm = nil
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Delete failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Deleted branch %s", msg.branch)
			if msg.pendingOk {
				m.pendingPreviewRepo = msg.repo
				m.pendingPreviewLine = msg.pendingLine
			}
		}
		delete(m.cache, msg.repo)
		return m, loadSnapshot(msg.repo, m.prLimit)

	case pushPRDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("PR failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Opened PR for %s", msg.branch)
		}
		delete(m.cache, msg.repo)
		return m, loadSnapshot(msg.repo, m.prLimit)

	case tea.KeyMsg:
		if m.deleteConfirm != nil {
			return m.handleDeleteDialogKey(msg)
		}
		if m.archiveConfirm != nil {
			return m.handleArchiveDialogKey(msg)
		}
		switch msg.String() {
		case "a", "A":
			return m.handleArchiveBranchRequest()
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
		case "tab", "shift+tab", "left", "right":
			if m.focus == focusTree {
				m.focus = focusPreview
			} else {
				m.focus = focusTree
			}
			return m, nil
		case "l", "L":
			m.prefs.ShowPreviewLinks = !m.prefs.ShowPreviewLinks
			_ = savePrefs(m.prefs)
			m.applyPreviewContent()
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
			return m.handlePreviewKey(msg)
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
	m.previewPaneH = contentH
	m.applyPreviewContent()
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

func (m *Model) currentRowKey() string {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return ""
	}
	r := m.rows[m.cursor]
	return fmt.Sprintf("%d:%s", r.Kind, r.Rel)
}

func (m *Model) resetPreviewLineForNewTreeRow() {
	k := m.currentRowKey()
	if k != m.lastPreviewRowKey {
		m.lastPreviewRowKey = k
		m.previewSelLine = 0
	}
}

func (m *Model) applyPreviewContent() {
	p := m.selectedAbs()
	contentH := m.previewPaneH
	if contentH <= 0 && m.height > 0 {
		contentH = m.height - 3
		if contentH < 6 {
			contentH = 6
		}
	}
	if contentH <= 0 {
		contentH = 6
	}

	m.previewHeader = ""

	plain := m.previewForSelection()
	plain = strings.ReplaceAll(plain, "\r\n", "\n")
	lines := strings.Split(plain, "\n")
	n := len(lines)
	if n == 0 {
		m.vp.Height = contentH
		m.vp.SetContent(plain)
		return
	}
	if m.previewSelLine < 0 {
		m.previewSelLine = 0
	}
	if m.previewSelLine >= n {
		m.previewSelLine = n - 1
	}

	// Repo with snapshot: fixed header (repo, checked out, worktrees, "Branches & open PRs") +
	// scrollable branch list so the header is never scrolled off-screen.
	if p != "" {
		if snap, ok := m.cache[p]; ok {
			treeStart := m.branchTreeContentStartLine(p, snap)
			if treeStart > 0 {
				headerPlain := m.snapshotHeaderBeforeTree(p, snap)
				scrollPlain := m.renderSnapshotScroll(p, snap)
				headerLines := treeStart
				scrollH := contentH - headerLines
				if scrollH < 3 {
					scrollH = 3
				}
				m.vp.Height = scrollH
				m.previewHeader = m.stylePreviewLinesAtGlobal(headerPlain, 0)
				m.vp.SetContent(m.stylePreviewLinesAtGlobal(scrollPlain, treeStart))
				m.ensurePreviewScrollVisibleSplit(treeStart)
				return
			}
		}
	}

	m.vp.Height = contentH
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("252"))
	for i := range lines {
		if i == m.previewSelLine {
			lines[i] = selStyle.Render(lines[i])
		}
	}
	m.vp.SetContent(strings.Join(lines, "\n"))
	m.ensurePreviewScrollVisibleFull()
}

func (m *Model) stylePreviewLinesAtGlobal(plain string, lineOffset int) string {
	plain = strings.ReplaceAll(plain, "\r\n", "\n")
	lines := strings.Split(plain, "\n")
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("252"))
	for i := range lines {
		if lineOffset+i == m.previewSelLine {
			lines[i] = selStyle.Render(lines[i])
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) ensurePreviewScrollVisibleFull() {
	n := m.vp.TotalLineCount()
	if n == 0 {
		return
	}
	h := m.vp.Height
	if h <= 0 {
		return
	}
	y := m.previewSelLine
	if y < m.vp.YOffset {
		m.vp.SetYOffset(y)
	}
	if y >= m.vp.YOffset+h {
		m.vp.SetYOffset(y - h + 1)
	}
}

func (m *Model) ensurePreviewScrollVisibleSplit(treeStart int) {
	if m.previewSelLine < treeStart {
		m.vp.SetYOffset(0)
		return
	}
	y := m.previewSelLine - treeStart
	n := m.vp.TotalLineCount()
	h := m.vp.Height
	if n == 0 || h <= 0 {
		return
	}
	if y < m.vp.YOffset {
		m.vp.SetYOffset(y)
	}
	if y >= m.vp.YOffset+h {
		m.vp.SetYOffset(y - h + 1)
	}
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
	b.WriteString(m.snapshotHeaderBeforeTree(path, s))
	webBase, _ := gitx.GitHubWebBase(path)
	b.WriteString(m.renderBranchTree(path, s, now, webBase))
	if s.ghErr != "" {
		fmt.Fprintf(&b, "\n  %s\n", s.ghErr)
		if hint := ghpr.AuthHint(); hint != "" {
			fmt.Fprintf(&b, "  %s\n", hint)
		}
	}
	return b.String()
}

// renderSnapshotScroll is the scrollable part of the preview (branch tree + gh errors), below the fixed header.
func (m *Model) renderSnapshotScroll(path string, s snapshot) string {
	now := time.Now()
	var b strings.Builder
	webBase, _ := gitx.GitHubWebBase(path)
	b.WriteString(m.renderBranchTree(path, s, now, webBase))
	if s.ghErr != "" {
		fmt.Fprintf(&b, "\n  %s\n", s.ghErr)
		if hint := ghpr.AuthHint(); hint != "" {
			fmt.Fprintf(&b, "  %s\n", hint)
		}
	}
	return b.String()
}

func (m *Model) renderBranchTree(repoPath string, s snapshot, now time.Time, webBase string) string {
	if s.gitErr != "" {
		return fmt.Sprintf("  error: %s\n", s.gitErr)
	}
	if !gitx.IsWorkTree(repoPath) {
		return "  (not a normal work tree)\n"
	}
	if len(s.unified) == 0 && len(s.prs) == 0 {
		return "  (no branches)\n"
	}
	rows, unmatched := m.branchRowsAndUnmatched(repoPath, s)
	var out strings.Builder
	for _, r := range rows {
		ind := strings.Repeat("  ", r.Depth)
		switch r.Kind {
		case branchtree.RowFolder:
			sym := "▾"
			if !m.isBranchFolderExpanded(repoPath, r.Rel) {
				sym = "▸"
			}
			fmt.Fprintf(&out, "%s%s %s\n", ind, sym, r.Label)
		case branchtree.RowBranch:
			if r.U == nil {
				continue
			}
			label := r.Label
			if u := gitx.BranchTreeURL(webBase, r.U.FullName); u != "" {
				label = m.linkText(u, r.Label)
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
				title = m.linkText(pr.URL, pr.Title)
			}
			head := fmt.Sprintf("#%d", pr.Number)
			head = m.linkText(pr.URL, head)
			fmt.Fprintf(&out, "%s◦ %s %s  %s  %s\n", ind, head, title, ghpr.FormatPRTagLine(*pr), lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render(opened))
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
				title = m.linkText(pr.URL, pr.Title)
			}
			head := fmt.Sprintf("#%d", pr.Number)
			head = m.linkText(pr.URL, head)
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
	sub := lipgloss.NewStyle().Foreground(lipgloss.Color("241")).Render("Space: expand/collapse · Enter: branches on GitHub · j/k: move")
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

func (m *Model) renderArchiveDialog() string {
	if m.archiveConfirm == nil {
		return ""
	}
	d := m.archiveConfirm
	onoff := "off"
	if d.dontAskAgain {
		onoff = "on"
	}
	w := m.width - 4
	if w > 100 {
		w = 100
	}
	body := fmt.Sprintf(
		"Archive branch %s?\n\nCreate tag %q at the branch tip, push the tag, then delete the branch on origin and locally.\n\n[y] confirm   [n] cancel   [space] don't ask again (%s)",
		d.branch, d.branch, onoff)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 2).
		Width(w).
		Render(body)
}

func (m *Model) renderDeleteDialog() string {
	if m.deleteConfirm == nil {
		return ""
	}
	d := m.deleteConfirm
	onoff := "off"
	if d.dontAskAgain {
		onoff = "on"
	}
	w := m.width - 4
	if w > 100 {
		w = 100
	}
	var body string
	if d.switchToFirst != "" {
		body = fmt.Sprintf(
			"Delete branch %s on origin and locally?\n\nYou have this branch checked out. Confirming will check out %q first, then remove the branch.\n\nThis does not create a tag (unlike archive).\n\n[y] confirm   [n] cancel   [space] don't ask again (%s)",
			d.branch, d.switchToFirst, onoff)
	} else {
		body = fmt.Sprintf(
			"Delete branch %s on origin and locally?\n\nThis does not create a tag (unlike archive). [y] confirm   [n] cancel   [space] don't ask again (%s)",
			d.branch, onoff)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("203")).
		Padding(1, 2).
		Width(w).
		Render(body)
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
	paneInnerH := m.previewPaneH
	if paneInnerH <= 0 {
		paneInnerH = m.vp.Height
	}
	leftPane := lipgloss.NewStyle().
		Border(border).
		BorderForeground(leftBorder).
		Width(m.leftColW).
		Height(paneInnerH + 2).
		Render(left)

	rightBody := m.rightPreviewBody()
	rightPane := lipgloss.NewStyle().
		Border(border).
		BorderForeground(func() lipgloss.Color {
			if m.focus == focusPreview {
				return lipgloss.Color("205")
			}
			return lipgloss.Color("240")
		}()).
		Width(m.vp.Width + 2).
		Height(paneInnerH + 2).
		Render(rightBody)

	row := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, "  ", rightPane)

	var stack []string
	stack = append(stack, row)
	if dlg := m.renderDeleteDialog(); dlg != "" {
		stack = append(stack, "", dlg)
	} else if dlg := m.renderArchiveDialog(); dlg != "" {
		stack = append(stack, "", dlg)
	}
	if m.statusMsg != "" {
		stack = append(stack, "", lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(m.statusMsg))
	}

	help := lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Render("tab/shift+tab/←/→: panes · L: preview links · preview: ↑/↓ · enter URL · c checkout · d delete · a archive · p push/PR · space folder · f/pgdn · shift+←/→ width · tree enter: branches on GitHub · g/G · r · q")

	stack = append(stack, "", help)

	return lipgloss.JoinVertical(lipgloss.Left, stack...)
}

func (m *Model) rightPreviewBody() string {
	if m.previewHeader != "" {
		return lipgloss.JoinVertical(lipgloss.Left, m.previewHeader, m.vp.View())
	}
	return m.vp.View()
}
