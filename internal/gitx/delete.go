package gitx

import (
	"bytes"
	"fmt"
	"strings"
)

// DefaultBranchToCheckout picks a local branch to switch to before deleting the checked-out branch.
// Tries main, master, origin/HEAD's branch, then any other local branch. exclude is skipped.
func DefaultBranchToCheckout(repo, exclude string) (string, error) {
	for _, name := range []string{"main", "master"} {
		if name == exclude {
			continue
		}
		if localBranchExists(repo, name) {
			return name, nil
		}
	}
	cmd := gitCmd(repo, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	if out, err := cmd.Output(); err == nil {
		ref := strings.TrimSpace(string(out))
		const p = "refs/remotes/origin/"
		if strings.HasPrefix(ref, p) {
			name := strings.TrimPrefix(ref, p)
			if name != "" && name != exclude && localBranchExists(repo, name) {
				return name, nil
			}
		}
	}
	refs, err := LocalBranches(repo)
	if err != nil {
		return "", fmt.Errorf("list branches: %w", err)
	}
	for _, r := range refs {
		if r.Name == exclude || r.Name == "" {
			continue
		}
		return r.Name, nil
	}
	return "", fmt.Errorf("no other local branch to switch to (create main or switch manually)")
}

// DeleteBranch removes the branch on origin (if present) and locally (if present).
// If that branch is currently checked out and switchToIfCheckedOut is non-empty, it runs Checkout
// to that branch first. If checked out and switchToIfCheckedOut is empty, it returns an error.
func DeleteBranch(repo, branch, switchToIfCheckedOut string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("empty branch name")
	}
	cur := CurrentBranch(repo)
	if cur == branch {
		if strings.TrimSpace(switchToIfCheckedOut) == "" {
			return fmt.Errorf("cannot delete: checked out on %q — switch to another branch first", branch)
		}
		if err := Checkout(repo, switchToIfCheckedOut); err != nil {
			return fmt.Errorf("checkout %q before delete: %w", switchToIfCheckedOut, err)
		}
	}
	hasRemote := remoteBranchExists(repo, branch)
	hasLocal := localBranchExists(repo, branch)
	if !hasRemote && !hasLocal {
		return fmt.Errorf("branch %q not found locally or on origin", branch)
	}
	var stderr bytes.Buffer
	if hasRemote {
		cmd := gitCmd(repo, "push", "origin", "--delete", "refs/heads/"+branch)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("delete remote branch: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		stderr.Reset()
	}
	if hasLocal {
		cmd := gitCmd(repo, "branch", "-D", branch)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("delete local branch: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}
	return nil
}
