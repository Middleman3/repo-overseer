package gitx

import (
	"bytes"
	"fmt"
	"strings"
)

// Checkout switches the work tree to branch using git switch.
func Checkout(repo, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("empty branch name")
	}
	if CurrentBranch(repo) == branch {
		return nil
	}
	cmd := gitCmd(repo, "switch", branch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git switch %q: %w: %s", branch, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
