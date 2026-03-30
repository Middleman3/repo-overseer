package gitx

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PushCreatePR pushes branch to origin when no remote tracking branch exists yet,
// then runs gh pr create with an empty body and the branch name as title.
// It does not switch branches, which avoids linked-worktree checkout conflicts.
func PushCreatePR(repo, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("empty branch name")
	}
	if !localBranchExists(repo, branch) {
		return fmt.Errorf("no local branch %q", branch)
	}
	if !remoteBranchExists(repo, branch) {
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
