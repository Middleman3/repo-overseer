package ghpr

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Merge merges a PR by number using a merge commit and deletes the remote branch.
func Merge(repo string, number int) error {
	if number <= 0 {
		return fmt.Errorf("invalid PR number: %d", number)
	}
	cmd := exec.Command("gh", "pr", "merge", strconv.Itoa(number), "--merge", "--delete-branch")
	cmd.Dir = repo
	cmd.Env = ghEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh pr merge #%d: %w: %s", number, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
