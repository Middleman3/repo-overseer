package ui

import "strings"

// OSC8 wraps text in a terminal hyperlink (iTerm2, VS Code, modern terminals).
func OSC8(url, text string) string {
	if url == "" {
		return text
	}
	const (
		osc = "\x1b]8;;"
		st  = "\x1b\\"
	)
	return osc + url + st + text + osc + st
}

// FirstOSC8URL returns the URI from the first OSC 8 hyperlink in s, or "".
// Matches the format produced by OSC8 (params empty: ESC ] 8 ; ; URI ST).
func FirstOSC8URL(s string) string {
	const prefix = "\x1b]8;;"
	const st = "\x1b\\"
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	rest := s[i+len(prefix):]
	j := strings.Index(rest, st)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
