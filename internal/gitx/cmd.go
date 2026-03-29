package gitx

import "os/exec"

// gitCmd runs git with Dir set to repo (works with older git than relying on -C).
func gitCmd(repo string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	return cmd
}
