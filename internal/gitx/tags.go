package gitx

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ListTags returns all tag names, sorted lexically.
func ListTags(repo string) ([]string, error) {
	cmd := gitCmd(repo, "tag", "-l")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag -l: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			tags = append(tags, line)
		}
	}
	sort.Strings(tags)
	return tags, nil
}

// TagTreeURL returns the GitHub tree URL for a tag name (same path rules as branches).
func TagTreeURL(webBase, tag string) string {
	if webBase == "" || tag == "" {
		return ""
	}
	segs := strings.Split(tag, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return webBase + "/tree/" + strings.Join(segs, "/")
}
