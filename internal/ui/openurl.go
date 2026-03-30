package ui

import (
	"os/exec"
	"runtime"

	tea "github.com/charmbracelet/bubbletea"
)

func openURLCmd(url string) tea.Cmd {
	return func() tea.Msg {
		_ = openURLInBrowser(url)
		return nil
	}
}

func openURLInBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
