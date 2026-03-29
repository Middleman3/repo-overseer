package ui

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
