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
	current   string
	unified   []gitx.UnifiedBranch
	prs       []ghpr.PR
	tags      []string // all lightweight tags (e.g. archive tags)
	worktrees []gitx.WorktreeInfo
	gitErr    string
	ghErr     string
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
	snapshotKey string
	err         error
}

// archiveConfirmDialog blocks the UI until the user confirms or cancels archive.
type archiveConfirmDialog struct {
	repo          string
	branch        string
	dontAskAgain  bool
	switchToFirst string // if non-empty, branch is checked out; confirm checks this out before archive
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
	snapshotKey string
	branch      string
	err         error
}

type mergePRDoneMsg struct {
	repo     string
	number   int
	snapshot string
	err      error
}

type worktreeRemoveDoneMsg struct {
	snapshotKey string
	err         error
}

// worktreeRemoveDialog is a multi-step confirm for removing a linked worktree.
type worktreeRemoveDialog struct {
	primary        string
	worktreeAbs    string
	branch         string // branch checked out in that worktree; empty if detached
	riskDetail     string // non-empty => show sync warning after first yes
	syncSecondStep bool   // branch sync / push warning dialog
	forceStep      bool   // dirty tree: confirm git worktree remove --force
	worktreeDirty  bool   // modified or untracked files in that checkout
}

const preloadWorkers = 4

// archivedTagsRel is the folder key for the collapsible "Archived tags" list (default collapsed).
const archivedTagsRel = "__archived_tags__"

// worktreesFolderRel is the folder key for the collapsible "Worktrees" section in preview.
const worktreesFolderRel = "__worktrees__"

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
	worktreeRemove     *worktreeRemoveDialog
	statusMsg          string
	width              int
	height             int
	pendingPreviewRepo string // repo path; cleared after snapshotMsg applies pending line
	pendingPreviewLine int    // absolute preview line; -1 means none
	previewPaneH       int    // last laid-out inner height for the right column (header + viewport)
	previewHeader      string // fixed region above the scrollable branch list (styled); empty if not split
	showInlineHelp     bool   // toggled with i; default off to avoid consuming terminal height
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
		leftFrac:       0.24,
		prefs:          loadPrefs(),
		showInlineHelp: false,
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
	case "m", "M":
		return m.handleMergePRRequest()
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// trimTrailingEmptyLines removes trailing "" entries from a Split result.
// Rendered preview text often ends with "\n", which produces a fake extra line
// and lets the cursor sit "below" the last visible row.
func trimTrailingEmptyLines(lines []string) []string {
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func (m *Model) previewEffectiveLineCount(plain string) int {
	plain = strings.ReplaceAll(plain, "\r\n", "\n")
	if plain == "" {
		return 0
	}
	lines := trimTrailingEmptyLines(strings.Split(plain, "\n"))
	return len(lines)
}

func (m *Model) previewPlainLineCount() int {
	return m.previewEffectiveLineCount(m.previewForSelection())
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
	if len(s.unified) == 0 && len(s.prs) == 0 && len(s.tags) == 0 && len(s.worktrees) <= 1 {
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
		case branchtree.RowTag:
			urls = append(urls, gitx.TagTreeURL(webBase, r.TagName))
		case branchtree.RowWorktreePath:
			urls = append(urls, "")
		case branchtree.RowWorktreeBranch:
			if r.U != nil {
				urls = append(urls, gitx.BranchTreeURL(webBase, r.U.FullName))
				continue
			}
			if r.BranchFullName != "" {
				urls = append(urls, gitx.BranchTreeURL(webBase, r.BranchFullName))
				continue
			}
			urls = append(urls, "")
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
	if folderRel == archivedTagsRel {
		if m.branchExpanded == nil {
			return false
		}
		v, ok := m.branchExpanded[branchFolderKey(repoAbs, folderRel)]
		if !ok {
			return false
		}
		return v
	}
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
	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Branches / PRs"))
	b.WriteString("\n")
	return b.String()
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

func (m *Model) appendArchivedTagRows(repoPath string, s snapshot, rows []branchtree.Row) []branchtree.Row {
	if len(s.tags) == 0 {
		return rows
	}
	rows = append(rows, branchtree.Row{
		Kind:  branchtree.RowFolder,
		Depth: 0,
		Rel:   archivedTagsRel,
		Label: "Archived tags",
	})
	if !m.isBranchFolderExpanded(repoPath, archivedTagsRel) {
		return rows
	}
	for _, tag := range s.tags {
		rows = append(rows, branchtree.Row{
			Kind:    branchtree.RowTag,
			Depth:   1,
			Rel:     archivedTagsRel,
			Label:   tag,
			TagName: tag,
		})
	}
	return rows
}

func (m *Model) buildBranchTreeRowsOnly(repoPath string, s snapshot) (rows []branchtree.Row, unmatched []ghpr.PR) {
	if s.gitErr != "" {
		return nil, nil
	}
	if !gitx.IsWorkTree(repoPath) {
		return nil, nil
	}
	if len(s.unified) > 0 || len(s.prs) > 0 {
		root := branchtree.Build(s.unified)
		var prs []ghpr.PR
		if s.ghErr == "" {
			prs = s.prs
		}
		unmatched = branchtree.AssignPRs(root, prs)
		branchtree.Flatten(root, 0, func(rel string) bool {
			return m.isBranchFolderExpanded(repoPath, rel)
		}, &rows)
	}
	rows = m.appendArchivedTagRows(repoPath, s, rows)
	if len(rows) == 0 {
		return nil, unmatched
	}
	return rows, unmatched
}

func (m *Model) prependWorktreeRows(repoPath string, s snapshot, base []branchtree.Row) []branchtree.Row {
	if len(s.worktrees) <= 1 || s.gitErr != "" {
		return base
	}
	var out []branchtree.Row
	out = append(out, branchtree.Row{
		Kind:  branchtree.RowFolder,
		Depth: 0,
		Rel:   worktreesFolderRel,
		Label: "Worktrees",
	})
	if !m.isBranchFolderExpanded(repoPath, worktreesFolderRel) {
		return append(out, base...)
	}
	for _, wt := range s.worktrees {
		isPri := filepath.Clean(wt.Path) == filepath.Clean(repoPath)
		label := m.relDisplay(wt.Path)
		if isPri {
			label += "  (primary)"
		}
		out = append(out, branchtree.Row{
			Kind:        branchtree.RowWorktreePath,
			Depth:       1,
			Label:       label,
			WorktreeAbs: wt.Path,
			IsPrimary:   isPri,
		})
		if wt.Detached || strings.TrimSpace(wt.Branch) == "" {
			out = append(out, branchtree.Row{
				Kind:        branchtree.RowWorktreeBranch,
				Depth:       2,
				Label:       "(detached)",
				WorktreeAbs: wt.Path,
				U:           nil,
			})
			continue
		}
		u := unifiedByFullName(s, wt.Branch)
		short := branchLabelName(wt.Branch)
		out = append(out, branchtree.Row{
			Kind:           branchtree.RowWorktreeBranch,
			Depth:          2,
			Label:          short,
			WorktreeAbs:    wt.Path,
			U:              u,
			BranchFullName: wt.Branch,
		})
	}
	return append(out, base...)
}

func (m *Model) branchRowsAndUnmatched(repoPath string, s snapshot) (rows []branchtree.Row, unmatched []ghpr.PR) {
	base, unmatched := m.buildBranchTreeRowsOnly(repoPath, s)
	return m.prependWorktreeRows(repoPath, s, base), unmatched
}

func unifiedByFullName(s snapshot, full string) *gitx.UnifiedBranch {
	for i := range s.unified {
		if s.unified[i].FullName == full {
			return &s.unified[i]
		}
	}
	return nil
}

func branchLabelName(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

func worktreeRemoveNeedsExtraConfirm(branch string, u *gitx.UnifiedBranch) bool {
	if strings.TrimSpace(branch) == "" {
		return true
	}
	if u == nil {
		return true
	}
	if u.Local == nil {
		return true
	}
	if u.Remote == nil {
		return true
	}
	if u.SyncErr != "" {
		return true
	}
	if u.Ahead > 0 || u.Behind > 0 {
		return true
	}
	return false
}

func worktreeRemoveRiskDetail(branch string, u *gitx.UnifiedBranch) string {
	var parts []string
	if strings.TrimSpace(branch) == "" {
		return "Detached HEAD or unknown branch."
	}
	if u == nil {
		return fmt.Sprintf("Could not load sync status for %q (missing from branch list).", branch)
	}
	if u.Local == nil {
		return fmt.Sprintf("No local ref for %q in this snapshot.", branch)
	}
	if u.Remote == nil {
		return "Branch has no corresponding origin/* ref (nothing pushed or no remote tracking)."
	}
	if u.SyncErr != "" {
		return fmt.Sprintf("Sync check failed: %s", u.SyncErr)
	}
	if u.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d local commit(s) not on origin", u.Ahead))
	}
	if u.Behind > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s) on origin not merged locally", u.Behind))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
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
	case branchtree.RowWorktreeBranch:
		if r.Label == "(detached)" {
			return "", "", false
		}
		wtDir := r.WorktreeAbs
		if wtDir == "" {
			return "", "", false
		}
		full := r.BranchFullName
		if r.U != nil {
			full = r.U.FullName
		}
		if full == "" {
			return "", "", false
		}
		return wtDir, full, true
	default:
		return "", "", false
	}
}

// isArchiveHotkey is true for the archive shortcut (a / A).
// Prefer matching on runes (robust across layouts) and ignore paste events.
func isArchiveHotkey(msg tea.KeyMsg) bool {
	if msg.Paste {
		return false
	}
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
		switch msg.Runes[0] {
		case 'a', 'A':
			return true
		}
	}
	s := msg.String()
	return s == "a" || s == "A"
}

func (m *Model) handleArchiveBranchRequest() (tea.Model, tea.Cmd) {
	repo, branch, ok := m.selectedPreviewBranchName()
	if !ok {
		return m, nil
	}
	m.statusMsg = ""
	switchTo := ""
	if gitx.CurrentBranch(repo) == branch {
		t, err := gitx.DefaultBranchToCheckout(repo, branch)
		if err != nil {
			m.statusMsg = fmt.Sprintf("Cannot archive: %v", err)
			return m, nil
		}
		switchTo = t
	}
	pl, pok := m.capturePendingPreviewAfterBranchRemoval(repo)
	if m.prefs.SkipArchiveConfirm {
		return m, m.runArchiveBranchCmd(repo, branch, switchTo, pl, pok)
	}
	m.archiveConfirm = &archiveConfirmDialog{repo: repo, branch: branch, switchToFirst: switchTo}
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
		return m, m.runArchiveBranchCmd(d.repo, d.branch, d.switchToFirst, pl, pok)
	case "n", "N", "esc", "q":
		m.archiveConfirm = nil
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) runArchiveBranchCmd(repo, branch, switchToFirst string, pendingLine int, pendingOk bool) tea.Cmd {
	return func() tea.Msg {
		err := gitx.ArchiveBranch(repo, branch, switchToFirst)
		return archiveDoneMsg{repo: repo, branch: branch, err: err, pendingLine: pendingLine, pendingOk: pendingOk}
	}
}

func (m *Model) handleDeleteBranchRequest() (tea.Model, tea.Cmd) {
	p := m.selectedAbs()
	if p == "" {
		return m, nil
	}
	snap, have := m.cache[p]
	if !have {
		return m, nil
	}
	start := m.branchTreeContentStartLine(p, snap)
	idx := m.previewSelLine - start
	rows, _ := m.branchRowsAndUnmatched(p, snap)
	if idx >= 0 && idx < len(rows) && rows[idx].Kind == branchtree.RowWorktreePath {
		return m.handleRemoveWorktreeRequest(rows[idx])
	}
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

func (m *Model) handleRemoveWorktreeRequest(row branchtree.Row) (tea.Model, tea.Cmd) {
	if row.Kind != branchtree.RowWorktreePath || row.WorktreeAbs == "" {
		return m, nil
	}
	if row.IsPrimary {
		m.statusMsg = "Cannot remove the primary worktree."
		return m, nil
	}
	primary := m.selectedAbs()
	if primary == "" {
		return m, nil
	}
	m.statusMsg = ""
	branch := gitx.CurrentBranch(row.WorktreeAbs)
	var u *gitx.UnifiedBranch
	if snap, ok := m.cache[primary]; ok {
		u = unifiedByFullName(snap, branch)
	}
	risk := ""
	if worktreeRemoveNeedsExtraConfirm(branch, u) {
		risk = worktreeRemoveRiskDetail(branch, u)
	}
	dirty := false
	if d, err := gitx.WorktreeHasLocalChanges(row.WorktreeAbs); err == nil {
		dirty = d
	}
	m.worktreeRemove = &worktreeRemoveDialog{
		primary:        primary,
		worktreeAbs:    row.WorktreeAbs,
		branch:         branch,
		riskDetail:     risk,
		syncSecondStep: false,
		forceStep:      false,
		worktreeDirty:  dirty,
	}
	return m, nil
}

func (m *Model) handleWorktreeRemoveDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.worktreeRemove == nil {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "y", "Y", "enter":
		d := m.worktreeRemove
		if d.forceStep {
			pri, wt := d.primary, d.worktreeAbs
			m.worktreeRemove = nil
			return m, m.runRemoveWorktreeCmd(pri, wt, true)
		}
		if d.syncSecondStep {
			if d.worktreeDirty {
				d.syncSecondStep = false
				d.forceStep = true
				return m, nil
			}
			pri, wt := d.primary, d.worktreeAbs
			m.worktreeRemove = nil
			return m, m.runRemoveWorktreeCmd(pri, wt, false)
		}
		if d.riskDetail != "" {
			d.syncSecondStep = true
			return m, nil
		}
		if d.worktreeDirty {
			d.forceStep = true
			return m, nil
		}
		pri, wt := d.primary, d.worktreeAbs
		m.worktreeRemove = nil
		return m, m.runRemoveWorktreeCmd(pri, wt, false)
	case "n", "N", "esc", "q":
		m.worktreeRemove = nil
		return m, nil
	default:
		return m, nil
	}
}

func (m *Model) runRemoveWorktreeCmd(primary, worktreeAbs string, force bool) tea.Cmd {
	return func() tea.Msg {
		err := gitx.RemoveWorktree(primary, worktreeAbs, force)
		return worktreeRemoveDoneMsg{snapshotKey: primary, err: err}
	}
}

func (m *Model) refreshWorktreesMap(primary string) {
	if m.worktreesByRepo == nil {
		m.worktreesByRepo = make(map[string][]string)
	}
	paths, err := gitx.ListWorktreePaths(primary)
	if err != nil || len(paths) == 0 {
		return
	}
	m.worktreesByRepo[primary] = gitx.SortPathsPrimaryFirst(paths, primary)
}

func (m *Model) handleCheckoutRequest() (tea.Model, tea.Cmd) {
	repo, branch, ok := m.selectedPreviewBranchName()
	if !ok {
		return m, nil
	}
	m.statusMsg = ""
	return m, m.runCheckoutCmd(repo, branch)
}

func (m *Model) runCheckoutCmd(wtDir, branch string) tea.Cmd {
	snapshotKey := m.selectedAbs()
	return func() tea.Msg {
		err := gitx.Checkout(wtDir, branch)
		return checkoutDoneMsg{snapshotKey: snapshotKey, err: err}
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

func (m *Model) runPushPRCmd(wtDir, branch string) tea.Cmd {
	snapshotKey := m.selectedAbs()
	return func() tea.Msg {
		err := gitx.PushCreatePR(wtDir, branch)
		return pushPRDoneMsg{snapshotKey: snapshotKey, branch: branch, err: err}
	}
}

func (m *Model) selectedPreviewPR() (repoAbs string, pr ghpr.PR, ok bool) {
	p := m.selectedAbs()
	if p == "" {
		return "", ghpr.PR{}, false
	}
	snap, have := m.cache[p]
	if !have {
		return "", ghpr.PR{}, false
	}
	start := m.branchTreeContentStartLine(p, snap)
	rows, _ := m.branchRowsAndUnmatched(p, snap)
	if len(rows) == 0 {
		return "", ghpr.PR{}, false
	}
	idx := m.previewSelLine - start
	if idx < 0 || idx >= len(rows) {
		return "", ghpr.PR{}, false
	}
	r := rows[idx]
	if r.Kind != branchtree.RowPR || r.PR == nil {
		return "", ghpr.PR{}, false
	}
	return p, *r.PR, true
}

func (m *Model) handleMergePRRequest() (tea.Model, tea.Cmd) {
	repo, pr, ok := m.selectedPreviewPR()
	if !ok {
		return m, nil
	}
	m.statusMsg = fmt.Sprintf("Merging PR #%d…", pr.Number)
	return m, m.runMergePRCmd(repo, pr.Number)
}

func (m *Model) runMergePRCmd(repo string, number int) tea.Cmd {
	snapshotKey := m.selectedAbs()
	return func() tea.Msg {
		err := ghpr.Merge(repo, number)
		return mergePRDoneMsg{repo: repo, number: number, snapshot: snapshotKey, err: err}
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
		delete(m.cache, msg.snapshotKey)
		return m, loadSnapshot(msg.snapshotKey, m.prLimit)

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
		delete(m.cache, msg.snapshotKey)
		return m, loadSnapshot(msg.snapshotKey, m.prLimit)

	case mergePRDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Merge failed: %v", msg.err)
		} else {
			m.statusMsg = fmt.Sprintf("Merged PR #%d", msg.number)
		}
		delete(m.cache, msg.repo)
		if msg.snapshot != "" {
			delete(m.cache, msg.snapshot)
			return m, loadSnapshot(msg.snapshot, m.prLimit)
		}
		return m, loadSnapshot(msg.repo, m.prLimit)

	case worktreeRemoveDoneMsg:
		m.worktreeRemove = nil
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("Worktree remove failed: %v", msg.err)
		} else {
			m.statusMsg = "Worktree removed."
			m.refreshWorktreesMap(msg.snapshotKey)
		}
		delete(m.cache, msg.snapshotKey)
		return m, loadSnapshot(msg.snapshotKey, m.prLimit)

	case tea.KeyMsg:
		if m.worktreeRemove != nil {
			return m.handleWorktreeRemoveDialogKey(msg)
		}
		if m.deleteConfirm != nil {
			return m.handleDeleteDialogKey(msg)
		}
		if m.archiveConfirm != nil {
			return m.handleArchiveDialogKey(msg)
		}
		if isArchiveHotkey(msg) {
			return m.handleArchiveBranchRequest()
		}
		switch msg.String() {
		case "shift+up":
			// Scroll the focused pane without moving the selection.
			if m.focus == focusPreview {
				m.vp.LineUp(1)
				return m, nil
			}
			if m.treeInnerH > 0 && len(m.rows) > 0 && m.scroll > 0 {
				m.scroll--
			}
			return m, nil
		case "shift+down":
			// Scroll the focused pane without moving the selection.
			if m.focus == focusPreview {
				m.vp.LineDown(1)
				return m, nil
			}
			if m.treeInnerH > 0 && len(m.rows) > 0 {
				maxScroll := len(m.rows) - m.treeInnerH
				if maxScroll < 0 {
					maxScroll = 0
				}
				if m.scroll < maxScroll {
					m.scroll++
				}
			}
			return m, nil
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
		case "c", "C":
			// If a transient status line is visible, treat c as "clear status" first.
			// When no status is visible, c keeps its existing meaning in preview mode (checkout).
			if m.statusMsg != "" {
				m.statusMsg = ""
				m.layout()
				return m, nil
			}
		case "i", "I":
			m.showInlineHelp = !m.showInlineHelp
			m.layout()
			return m, nil
		case "r":
			m.statusMsg = "Refreshing all repositories…"
			m.vp.SetContent("Refreshing all repositories…")
			return m, fetchAllRepos(m.allRepos)
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
	// Reserve space for bottom blocks (dialogs/status/help) so panes never exceed terminal height.
	contentH := m.height - 3 - m.bottomContentHeight()
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
		contentH = m.height - 3 - m.bottomContentHeight()
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
	nEff := len(trimTrailingEmptyLines(append([]string(nil), lines...)))
	if n == 0 || nEff == 0 {
		m.vp.Height = contentH
		m.vp.SetContent(plain)
		return
	}
	if m.previewSelLine < 0 {
		m.previewSelLine = 0
	}
	// Do not allow selection on trailing blank lines from a terminal \n.
	if m.previewSelLine >= nEff {
		m.previewSelLine = nEff - 1
	}

	// Repo with snapshot: fixed header (repo, checked out, "Branches / PRs") +
	// scrollable preview (worktrees block + branch list + …) so the header is never scrolled off-screen.
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

func (m *Model) renderInlineHelp() string {
	helpW := m.width - 4
	if helpW < 24 {
		helpW = 24
	}
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("241")).
		Width(helpW).
		Render("i: hide controls · tab/shift+tab/←/→: panes · L: preview links · preview: ↑/↓ · enter URL · c checkout · d delete branch / worktree path · A archive · p push/PR · m merge PR · space folder · f/pgdn · shift+↑/↓ scroll pane · shift+←/→ width · tree enter: branches on GitHub · g/G · r refresh all · q")
}

func (m *Model) bottomContentHeight() int {
	if m.width <= 0 {
		return 0
	}
	total := 0
	addBlock := func(block string) {
		if block == "" {
			return
		}
		// View inserts a blank separator before every bottom block.
		total++
		total += lipgloss.Height(block)
	}

	if dlg := m.renderWorktreeRemoveDialog(); dlg != "" {
		addBlock(dlg)
	} else if dlg := m.renderDeleteDialog(); dlg != "" {
		addBlock(dlg)
	} else if dlg := m.renderArchiveDialog(); dlg != "" {
		addBlock(dlg)
	}
	if m.statusMsg != "" {
		statusW := m.width - 4
		if statusW < 24 {
			statusW = 24
		}
		status := lipgloss.NewStyle().
			Foreground(lipgloss.Color("214")).
			Width(statusW).
			Render(m.statusMsg)
		addBlock(status)
	}
	if m.showInlineHelp {
		addBlock(m.renderInlineHelp())
	}
	return total
}

func (m *Model) stylePreviewLinesAtGlobal(plain string, lineOffset int) string {
	plain = strings.ReplaceAll(plain, "\r\n", "\n")
	// Avoid creating a phantom extra line when the source ends with "\n".
	// This is important in split-header mode where headerLines is used to size
	// the viewport: an extra empty line would push content off-screen.
	plain = strings.TrimSuffix(plain, "\n")
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
	rows, unmatched := m.branchRowsAndUnmatched(repoPath, s)
	if len(rows) == 0 && len(unmatched) == 0 {
		return "  (no branches)\n"
	}
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
		case branchtree.RowTag:
			label := r.Label
			if u := gitx.TagTreeURL(webBase, r.TagName); u != "" {
				label = m.linkText(u, r.Label)
			}
			fmt.Fprintf(&out, "%s◦ %s\n", ind, label)
		case branchtree.RowWorktreePath:
			fmt.Fprintf(&out, "%s◦ %s\n", ind, r.Label)
		case branchtree.RowWorktreeBranch:
			if r.U == nil {
				fmt.Fprintf(&out, "%s◦ %s\n", ind, r.Label)
				break
			}
			label := r.Label
			if u := gitx.BranchTreeURL(webBase, r.U.FullName); u != "" {
				label = m.linkText(u, r.Label)
			}
			fmt.Fprintf(&out, "%s◦ %s  %s\n", ind, label, formatUnifiedBranchMeta(r.U, now))
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
		if tags, err := gitx.ListTags(path); err == nil {
			s.tags = tags
		}
		if wts, err := gitx.ListWorktreesDetail(path); err == nil {
			s.worktrees = wts
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
	var body string
	if d.switchToFirst != "" {
		body = fmt.Sprintf(
			"Archive branch %s?\n\nYou have this branch checked out. Confirming will check out %q first, then create tag %q at the branch tip, push the tag, and delete the branch on origin and locally.\n\n[y] confirm   [n] cancel   [space] don't ask again (%s)",
			d.branch, d.switchToFirst, d.branch, onoff)
	} else {
		body = fmt.Sprintf(
			"Archive branch %s?\n\nCreate tag %q at the branch tip, push the tag, then delete the branch on origin and locally.\n\n[y] confirm   [n] cancel   [space] don't ask again (%s)",
			d.branch, d.branch, onoff)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("205")).
		Padding(1, 2).
		Width(w).
		Render(body)
}

func (m *Model) renderWorktreeRemoveDialog() string {
	if m.worktreeRemove == nil {
		return ""
	}
	d := m.worktreeRemove
	w := m.width - 4
	if w > 100 {
		w = 100
	}
	pathLabel := m.relDisplay(d.worktreeAbs)
	var body string
	switch {
	case d.forceStep:
		body = fmt.Sprintf(
			"This checkout has modified, staged, or untracked files. Git requires --force to drop the worktree link.\n\nThat is separate from whether the branch is merged on the remote: local files still block a normal remove.\n\nForce-remove worktree %q? (Uncommitted files there may be deleted with the directory; the branch on origin is unchanged.)\n\n[y] confirm   [n] cancel",
			pathLabel,
		)
	case d.syncSecondStep:
		head := fmt.Sprintf("Branch %q is not fully pushed or not up to date with origin.", d.branch)
		if strings.TrimSpace(d.branch) == "" {
			head = "This worktree is not on a named branch or sync status is unknown."
		}
		body = fmt.Sprintf(
			"%s\n\n%s\n\nRemove worktree %q anyway?\n\n[y] confirm   [n] cancel",
			head,
			d.riskDetail,
			pathLabel,
		)
	default:
		hint := ""
		if d.worktreeDirty {
			hint = "\n\nThis checkout has local changes; you will be asked to confirm a force remove next."
		}
		body = fmt.Sprintf(
			"Remove linked worktree %q?\n\nThis detaches the extra checkout from the repo (git worktree remove).%s\n\n[y] confirm   [n] cancel",
			pathLabel,
			hint,
		)
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("214")).
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
	if dlg := m.renderWorktreeRemoveDialog(); dlg != "" {
		stack = append(stack, "", dlg)
	} else if dlg := m.renderDeleteDialog(); dlg != "" {
		stack = append(stack, "", dlg)
	} else if dlg := m.renderArchiveDialog(); dlg != "" {
		stack = append(stack, "", dlg)
	}
	if m.statusMsg != "" {
		statusW := m.width - 4
		if statusW < 24 {
			statusW = 24
		}
		stack = append(stack, "",
			lipgloss.NewStyle().
				Foreground(lipgloss.Color("214")).
				Width(statusW).
				Render(m.statusMsg))
	}

	if m.showInlineHelp {
		stack = append(stack, "", m.renderInlineHelp())
	}

	frame := lipgloss.JoinVertical(lipgloss.Left, stack...)
	return m.fitFrameToTerminal(frame)
}

func (m *Model) fitFrameToTerminal(frame string) string {
	if m.height <= 0 {
		return frame
	}
	lines := strings.Split(frame, "\n")
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) rightPreviewBody() string {
	if m.previewHeader != "" {
		return lipgloss.JoinVertical(lipgloss.Left, m.previewHeader, m.vp.View())
	}
	return m.vp.View()
}
