package gitx

import (
	"bytes"
	"fmt"
	"strings"
)

// ArchiveBranch creates a tag named after the branch at the branch tip, pushes the tag,
// then deletes the branch on origin (if present) and locally (if present).
// Fails if that branch is currently checked out, or if a tag with the branch name already exists.
func ArchiveBranch(repo, branch string) error {
	if branch == "" {
		return fmt.Errorf("empty branch name")
	}
	cur := CurrentBranch(repo)
	if cur == branch {
		return fmt.Errorf("cannot archive: checked out on %q — switch to another branch first", branch)
	}

	sha, err := resolveBranchTip(repo, branch)
	if err != nil {
		return err
	}

	tag := branch
	if refExists(repo, "refs/tags/"+tag) {
		return fmt.Errorf("tag %q already exists (delete it or pick another branch)", tag)
	}

	cmd := gitCmd(repo, "tag", tag, sha)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git tag: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	stderr.Reset()
	cmd = gitCmd(repo, "push", "origin", tag)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = gitCmd(repo, "tag", "-d", tag).Run()
		return fmt.Errorf("push tag: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	if remoteBranchExists(repo, branch) {
		stderr.Reset()
		cmd = gitCmd(repo, "push", "origin", "--delete", branch)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("delete remote branch: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}

	if localBranchExists(repo, branch) {
		stderr.Reset()
		cmd = gitCmd(repo, "branch", "-D", branch)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("delete local branch: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
	}

	return nil
}

func refExists(repo, ref string) bool {
	cmd := gitCmd(repo, "rev-parse", "-q", "--verify", ref)
	return cmd.Run() == nil
}

func localBranchExists(repo, branch string) bool {
	return refExists(repo, "refs/heads/"+branch)
}

func remoteBranchExists(repo, branch string) bool {
	return refExists(repo, "refs/remotes/origin/"+branch)
}

func resolveBranchTip(repo, branch string) (string, error) {
	if localBranchExists(repo, branch) {
		cmd := gitCmd(repo, "rev-parse", "refs/heads/"+branch)
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	if remoteBranchExists(repo, branch) {
		cmd := gitCmd(repo, "rev-parse", "refs/remotes/origin/"+branch)
		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", fmt.Errorf("branch %q not found locally or on origin", branch)
}
