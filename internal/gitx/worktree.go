package gitx

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GitCommonDirAbs returns the absolute path to the shared .git directory for this work tree.
func GitCommonDirAbs(repo string) (string, error) {
	cmd := gitCmd(repo, "rev-parse", "--git-common-dir")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", fmt.Errorf("empty git-common-dir")
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}
	return filepath.Clean(filepath.Join(repo, raw)), nil
}

// DotGitIsDir reports whether .git is a directory (main worktree) vs a gitfile (linked worktree).
func DotGitIsDir(repo string) bool {
	fi, err := os.Stat(filepath.Join(repo, ".git"))
	if err != nil {
		return false
	}
	return fi.IsDir()
}

// WorktreeInfo is one worktree from `git worktree list --porcelain`.
type WorktreeInfo struct {
	Path     string // absolute or normalized checkout path
	Head     string // revparse target (usually a hex SHA)
	Branch   string // short branch name (no refs/heads/); empty when detached or missing
	Detached bool
}

// ListWorktreesDetail parses `git worktree list --porcelain` for paths, HEAD, and branch state.
func ListWorktreesDetail(repo string) ([]WorktreeInfo, error) {
	cmd := gitCmd(repo, "worktree", "list", "--porcelain")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var blocks [][]string
	var cur []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			if len(cur) > 0 {
				blocks = append(blocks, cur)
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		blocks = append(blocks, cur)
	}
	var infos []WorktreeInfo
	for _, b := range blocks {
		var info WorktreeInfo
		for _, raw := range b {
			line := strings.TrimSpace(raw)
			switch {
			case strings.HasPrefix(line, "worktree "):
				p := strings.TrimSpace(strings.TrimPrefix(line, "worktree"))
				if p != "" {
					info.Path = filepath.Clean(p)
				}
			case strings.HasPrefix(line, "HEAD "):
				info.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
			case strings.HasPrefix(line, "branch "):
				ref := strings.TrimSpace(strings.TrimPrefix(line, "branch "))
				info.Detached = false
				if strings.HasPrefix(ref, "refs/heads/") {
					info.Branch = strings.TrimPrefix(ref, "refs/heads/")
				} else if ref != "" {
					info.Branch = ref
				}
			case line == "detached":
				info.Detached = true
			}
		}
		if info.Path != "" {
			infos = append(infos, info)
		}
	}
	return infos, nil
}

// WorktreeHasLocalChanges reports whether worktreePath has a non-clean working tree
// (modified, staged, or untracked files). Used to decide if remove needs --force.
func WorktreeHasLocalChanges(worktreePath string) (bool, error) {
	worktreePath = filepath.Clean(worktreePath)
	if worktreePath == "" {
		return false, fmt.Errorf("empty worktree path")
	}
	cmd := gitCmd(worktreePath, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// RemoveWorktree runs git worktree remove from the primary (or any) worktree of the repo.
// If force is true, runs with --force (required when the checkout has local modifications).
func RemoveWorktree(primaryRepo, worktreePath string, force bool) error {
	worktreePath = filepath.Clean(worktreePath)
	if worktreePath == "" {
		return fmt.Errorf("empty worktree path")
	}
	primaryRepo = filepath.Clean(primaryRepo)
	args := []string{"worktree", "remove", worktreePath}
	if force {
		args = []string{"worktree", "remove", "--force", worktreePath}
	}

	// Fast path: use the selected repository context.
	if err := runWorktreeRemove(primaryRepo, args); err == nil {
		return nil
	} else {
		// Fallback: some external tools (including Cursor) can create worktrees under a
		// different primary repo than the one selected in the UI. Retry from the target
		// worktree's own git-common-dir root when it differs.
		if fallbackRepo := inferRepoRootFromWorktree(worktreePath); fallbackRepo != "" && fallbackRepo != primaryRepo {
			if fallbackErr := runWorktreeRemove(fallbackRepo, args); fallbackErr == nil {
				return nil
			}
		}
		return err
	}
}

func runWorktreeRemove(repo string, args []string) error {
	cmd := gitCmd(repo, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git worktree remove %q: %w: %s", args[len(args)-1], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func inferRepoRootFromWorktree(worktreePath string) string {
	commonDir, err := GitCommonDirAbs(worktreePath)
	if err != nil {
		return ""
	}
	// Typical value is "<repo>/.git". Use parent as the repository root.
	if filepath.Base(commonDir) == ".git" {
		return filepath.Clean(filepath.Dir(commonDir))
	}
	return ""
}

// ListWorktreePaths returns linked worktree paths in git's order (main worktree first).
func ListWorktreePaths(repo string) ([]string, error) {
	infos, err := ListWorktreesDetail(repo)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(infos))
	for i := range infos {
		paths[i] = infos[i].Path
	}
	return paths, nil
}

// PickPrimaryWorktree chooses the main worktree path among paths sharing the same GitCommonDir.
func PickPrimaryWorktree(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	for _, p := range paths {
		if DotGitIsDir(p) {
			return p
		}
	}
	wt, err := ListWorktreePaths(paths[0])
	if err == nil && len(wt) > 0 {
		for _, first := range wt {
			first = filepath.Clean(first)
			for _, p := range paths {
				if filepath.Clean(p) == first {
					return p
				}
			}
		}
	}
	sort.Strings(paths)
	return paths[0]
}

// SortPathsPrimaryFirst places primary first, then remaining paths sorted.
func SortPathsPrimaryFirst(paths []string, primary string) []string {
	return sortPathsPrimaryFirst(paths, primary)
}

func sortPathsPrimaryFirst(paths []string, primary string) []string {
	seen := map[string]bool{}
	var rest []string
	for _, p := range paths {
		p = filepath.Clean(p)
		if p == filepath.Clean(primary) {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		rest = append(rest, p)
	}
	sort.Strings(rest)
	out := []string{filepath.Clean(primary)}
	out = append(out, rest...)
	return out
}

// DedupeWorktreeRoots groups paths that share the same git common dir and returns one primary
// path per group plus a map from primary to all paths (primary first).
func DedupeWorktreeRoots(paths []string) (primaries []string, byPrimary map[string][]string) {
	byPrimary = make(map[string][]string)
	groups := make(map[string][]string)

	for _, p := range paths {
		p = filepath.Clean(p)
		cd, e := GitCommonDirAbs(p)
		if e != nil {
			groups[p] = []string{p}
			continue
		}
		groups[cd] = append(groups[cd], p)
	}

	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		pri := PickPrimaryWorktree(group)
		sorted := sortPathsPrimaryFirst(group, pri)
		primaries = append(primaries, pri)
		byPrimary[pri] = sorted
	}
	sort.Strings(primaries)
	return primaries, byPrimary
}
