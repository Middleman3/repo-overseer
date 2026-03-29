package gitx

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// UnifiedBranch merges local and origin remote tracking for one branch name (no origin/ prefix).
type UnifiedBranch struct {
	FullName string // e.g. main, feature/foo
	Local    *Ref   // refs/heads/… short name
	Remote   *Ref   // origin side (we omit origin/ in UI)
	Ahead    int    // commits on local not on remote (-1 = n/a)
	Behind   int    // commits on remote not on local (-1 = n/a)
	SyncErr  string // if rev-list failed
}

// CollectUnifiedBranches builds one row per logical branch name (union of locals and origin/*).
func CollectUnifiedBranches(repo string) ([]UnifiedBranch, error) {
	locals, err := LocalBranches(repo)
	if err != nil {
		return nil, err
	}
	remotes, err := OriginRemoteBranches(repo)
	if err != nil {
		return nil, err
	}

	localBy := make(map[string]Ref)
	for _, r := range locals {
		localBy[r.Name] = r
	}

	remoteBy := make(map[string]Ref)
	for _, r := range remotes {
		short := strings.TrimPrefix(r.Name, "origin/")
		if short == "" || short == r.Name {
			// e.g. malformed "origin" without branch — skip
			continue
		}
		remoteBy[short] = r
	}

	names := make(map[string]struct{})
	for k := range localBy {
		names[k] = struct{}{}
	}
	for k := range remoteBy {
		names[k] = struct{}{}
	}

	keys := make([]string, 0, len(names))
	for name := range names {
		keys = append(keys, name)
	}
	sort.Strings(keys)

	out := make([]UnifiedBranch, 0, len(keys))
	for _, name := range keys {
		u := UnifiedBranch{FullName: name}
		if lr, ok := localBy[name]; ok {
			lr := lr
			u.Local = &lr
		}
		if rr, ok := remoteBy[name]; ok {
			rr := rr
			u.Remote = &rr
		}
		if u.Local != nil && u.Remote != nil {
			ahead, behind, err := LeftRightCount(repo,
				"refs/heads/"+name,
				"refs/remotes/origin/"+name,
			)
			if err != nil {
				u.SyncErr = err.Error()
				u.Ahead, u.Behind = -1, -1
			} else {
				u.Ahead, u.Behind = ahead, behind
			}
		} else {
			u.Ahead, u.Behind = -1, -1
		}
		out = append(out, u)
	}

	return out, nil
}

// LeftRightCount runs git rev-list --left-right --count left...right (ahead | behind).
func LeftRightCount(repo, leftRef, rightRef string) (ahead, behind int, err error) {
	arg := leftRef + "..." + rightRef
	cmd := gitCmd(repo, "rev-list", "--left-right", "--count", arg)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("rev-list: unexpected output: %q", strings.TrimSpace(string(out)))
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, err
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, err
	}
	return ahead, behind, nil
}
