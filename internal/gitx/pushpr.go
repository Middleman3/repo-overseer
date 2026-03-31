package gitx

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PushCreatePR opens a PR for branch with an empty body and branch name as title.
// If a local branch exists and origin/<branch> does not, it pushes first.
// If only origin/<branch> exists, it creates the PR directly from that remote head.
// It does not switch branches, which avoids linked-worktree checkout conflicts.
func PushCreatePR(repo, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("empty branch name")
	}
	hasLocal := localBranchExists(repo, branch)
	hasRemote := remoteBranchExists(repo, branch)
	if !hasLocal && !hasRemote {
		return fmt.Errorf("branch %q not found locally or on origin", branch)
	}
	if hasLocal && !hasRemote {
		cmd := gitCmd(repo, "push", "-u", "origin", branch)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git push -u origin %q: %w: %s", branch, err, strings.TrimSpace(stderr.String()))
		}
	}
	gh := exec.Command("gh", "pr", "create", "--head", branch, "--title", branch, "--body", "")
	gh.Dir = repo
	gh.Env = os.Environ()
	var stderr bytes.Buffer
	gh.Stderr = &stderr
	if err := gh.Run(); err != nil {
		return fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
