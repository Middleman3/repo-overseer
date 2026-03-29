package gitx

import (
	"fmt"
	"net/url"
	"strings"
)

// GitHubWebBase returns the https base URL for the origin remote (e.g. https://github.com/o/r), or "" if not GitHub.
func GitHubWebBase(repo string) (string, error) {
	raw, err := remoteGetURL(repo, "origin")
	if err != nil {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty origin URL")
	}
	// git@github.com:owner/repo.git
	if strings.HasPrefix(raw, "git@github.com:") {
		rest := strings.TrimPrefix(raw, "git@github.com:")
		rest = strings.TrimSuffix(rest, ".git")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("unparseable ssh URL")
		}
		return "https://github.com/" + parts[0] + "/" + parts[1], nil
	}
	// https://github.com/o/r or https://github.com/o/r.git
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" {
		return "", fmt.Errorf("not github.com: %s", host)
	}
	p := strings.Trim(u.Path, "/")
	p = strings.TrimSuffix(p, ".git")
	if p == "" {
		return "", fmt.Errorf("no path in URL")
	}
	return "https://github.com/" + p, nil
}

func remoteGetURL(repo, name string) (string, error) {
	cmd := gitCmd(repo, "remote", "get-url", name)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// BranchTreeURL returns the GitHub tree URL for a branch name, or "" if base is empty.
func BranchTreeURL(webBase, branch string) string {
	if webBase == "" || branch == "" {
		return ""
	}
	// GitHub uses /tree/BRANCH — encode path segments for slashes in branch names
	segs := strings.Split(branch, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return webBase + "/tree/" + strings.Join(segs, "/")
}
