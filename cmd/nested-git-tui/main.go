package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"nested-git-tui/internal/scan"
	"nested-git-tui/internal/ui"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	root := flag.String("root", ".", "directory to scan for nested git repositories")
	prLimit := flag.Int("pr-limit", 200, "max open PRs to fetch per repository (gh)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Interactive TUI for nested git repos (branches, origin/*, open PRs).\n")
		fmt.Fprintf(os.Stderr, "Requires git and gh on PATH. For GitHub API access, set GH_TOKEN or GITHUB_TOKEN,\n")
		fmt.Fprintf(os.Stderr, "or run `gh auth login`. Tokens are read from the process environment.\n")
		fmt.Fprintf(os.Stderr, "Startup: runs `git fetch origin --prune` in every listed repo, then preloads branch/PR data.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "root path: %v\n", err)
		os.Exit(1)
	}

	warnIfGHUnauthenticated()

	repos, worktreesByPrimary, err := scan.GitRoots(rootAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
		os.Exit(1)
	}
	if len(repos) == 0 {
		fmt.Fprintln(os.Stderr, "no git repositories found under", rootAbs)
		os.Exit(1)
	}

	m := ui.New(rootAbs, repos, worktreesByPrimary, *prLimit)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func warnIfGHUnauthenticated() {
	if os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("GH_TOKEN") != "" {
		return
	}
	cmd := exec.Command("gh", "auth", "status")
	cmd.Env = os.Environ()
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "nested-git-tui: gh is not logged in and no GITHUB_TOKEN/GH_TOKEN is set; PR data may fail. Run `gh auth login` or export a token.")
	}
}
