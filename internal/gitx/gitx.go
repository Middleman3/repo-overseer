package gitx

import (
	"bytes"
	"fmt"
	"strings"
	"time"
)

// Ref is a git ref with a best-effort creation/update timestamp from for-each-ref.
type Ref struct {
	Name string
	When time.Time
	Raw  string // original creatordate string if parsing fails
}

// CurrentBranch returns the checked-out branch name, or empty if detached/empty.
func CurrentBranch(repo string) string {
	cmd := gitCmd(repo, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func forEachRef(repo string, pattern string) ([]Ref, error) {
	cmd := gitCmd(repo, "for-each-ref",
		"--sort=-creatordate",
		"--format=%(creatordate:iso8601)\t%(refname:short)",
		pattern,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseRefLines(string(out))
}

func parseRefLines(s string) ([]Ref, error) {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	var refs []Ref
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		ts := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		when, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			when, err = time.Parse("2006-01-02 15:04:05 -0700", ts)
		}
		r := Ref{Name: name, Raw: ts}
		if err == nil {
			r.When = when
		}
		refs = append(refs, r)
	}
	return refs, nil
}

// LocalBranches lists refs/heads with creatordate.
func LocalBranches(repo string) ([]Ref, error) {
	return forEachRef(repo, "refs/heads/")
}

// OriginRemoteBranches lists refs/remotes/origin except origin/HEAD.
func OriginRemoteBranches(repo string) ([]Ref, error) {
	refs, err := forEachRef(repo, "refs/remotes/origin")
	if err != nil {
		return nil, err
	}
	var out []Ref
	for _, r := range refs {
		if r.Name == "origin/HEAD" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// IsWorkTree reports whether repo looks like a normal work tree.
func IsWorkTree(repo string) bool {
	cmd := gitCmd(repo, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}
