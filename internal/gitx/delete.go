package gitx

import (
	"bytes"
	"fmt"
	"strings"
)

// DeleteBranch removes the branch on origin (if present) and locally (if present).
// Fails if that branch is currently checked out, or if the branch does not exist locally or on origin.
func DeleteBranch(repo, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("empty branch name")
	}
	cur := CurrentBranch(repo)
	if cur == branch {
		return fmt.Errorf("cannot delete: checked out on %q — switch to another branch first", branch)
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
