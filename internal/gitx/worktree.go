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

// ListWorktreePaths returns linked worktree paths in git's order (main worktree first).
func ListWorktreePaths(repo string) ([]string, error) {
	cmd := gitCmd(repo, "worktree", "list", "--porcelain")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree"))
			if p != "" {
				paths = append(paths, filepath.Clean(p))
			}
		}
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
