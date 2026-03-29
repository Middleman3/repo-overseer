package timefmt

import (
	"fmt"
	"strings"
	"time"
)

// Relative renders a concise English relative time (e.g. "3d ago", "just now").
func Relative(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = -d
		t, now = now, t
		d = now.Sub(t)
	}
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		return fmt.Sprintf("%dm ago", m)
	}
	if d < 24*time.Hour {
		h := int(d / time.Hour)
		return fmt.Sprintf("%dh ago", h)
	}
	if d < 14*24*time.Hour {
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd ago", days)
	}
	if d < 365*24*time.Hour {
		weeks := int(d / (7 * 24 * time.Hour))
		return fmt.Sprintf("%dw ago", weeks)
	}
	years := int(d / (365 * 24 * time.Hour))
	if years < 1 {
		years = 1
	}
	return fmt.Sprintf("%dy ago", years)
}

// ParseGitHub parses RFC3339 timestamps from the GitHub API / gh --json.
func ParseGitHub(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
