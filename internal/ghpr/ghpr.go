package ghpr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// PR is a subset of `gh pr list --json` for open pull requests.
type PR struct {
	Number            int              `json:"number"`
	Title             string           `json:"title"`
	HeadRefName       string           `json:"headRefName"`
	BaseRefName       string           `json:"baseRefName"`
	CreatedAt         string           `json:"createdAt"`
	MergeStateStatus  string           `json:"mergeStateStatus"`
	URL               string           `json:"url"`
	StatusCheckRollup []statusCheckRun `json:"statusCheckRollup"`
}

type statusCheckRun struct {
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

func ghEnv() []string {
	// Inherit the parent environment so GITHUB_TOKEN, GH_TOKEN, PATH, and gh config dirs work.
	return os.Environ()
}

// RepoViewable returns nil if `gh repo view` works for this path (GitHub + auth).
func RepoViewable(repo string) error {
	cmd := exec.Command("gh", "repo", "view")
	cmd.Dir = repo
	cmd.Env = ghEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// ListOpen returns open PRs via gh (limit caps per-repo fetch).
func ListOpen(repo string, limit int) ([]PR, error) {
	if limit <= 0 {
		limit = 200
	}
	cmd := exec.Command("gh", "pr", "list",
		"--state", "open",
		"--limit", strconv.Itoa(limit),
		"--json", "number,title,headRefName,baseRefName,createdAt,mergeStateStatus,statusCheckRollup,url",
	)
	cmd.Dir = repo
	cmd.Env = ghEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh json: %w", err)
	}
	return prs, nil
}

// AuthHint returns extra guidance when gh likely failed for auth.
func AuthHint() string {
	hasGH := os.Getenv("GH_TOKEN") != ""
	hasG := os.Getenv("GITHUB_TOKEN") != ""
	if hasGH || hasG {
		return ""
	}
	return "Set GH_TOKEN or GITHUB_TOKEN in the environment, or run `gh auth login`. " +
		"If your org stores tokens in AWS Secrets Manager (e.g. secret `devops`), export the GitHub token before launching."
}
