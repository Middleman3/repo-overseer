package ghpr

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// HeadBranchLocalName returns the branch name on this repo (after fork owner: prefix).
func HeadBranchLocalName(headRefName string) string {
	if i := strings.LastIndex(headRefName, ":"); i >= 0 {
		return headRefName[i+1:]
	}
	return headRefName
}

// CheckPassedFailedTotal counts completed checks: passed (SUCCESS/SKIPPED), failed (other conclusions).
// Total is always len(StatusCheckRollup); in-flight checks are included in total but not in passed/failed until complete.
func CheckPassedFailedTotal(pr PR) (passed, failed, total int) {
	total = len(pr.StatusCheckRollup)
	for _, c := range pr.StatusCheckRollup {
		if c.Status != "COMPLETED" {
			continue
		}
		if c.Conclusion == nil {
			continue
		}
		switch *c.Conclusion {
		case "SUCCESS", "SKIPPED":
			passed++
		default:
			failed++
		}
	}
	return passed, failed, total
}

// MergeTag returns a short label for merge state (for tag display).
func MergeTag(status string) string {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "CLEAN":
		return "clean"
	case "BLOCKED", "UNSTABLE":
		return "blocked"
	case "BEHIND":
		return "behind"
	case "DIRTY":
		return "dirty"
	case "UNKNOWN", "":
		return "?"
	default:
		return strings.ToLower(status)
	}
}

// FormatPRTagLine returns compact tag-style metadata for one PR line (no newlines).
func FormatPRTagLine(pr PR) string {
	merge := MergeTag(pr.MergeStateStatus)
	pill := lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("249")).Padding(0, 1)
	mergeS := pill.Render("merge:" + merge)

	passed, failed, total := CheckPassedFailedTotal(pr)
	// (2✓ 0✗ / 5) — green check count, red x count, gray total
	passPart := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render(fmt.Sprintf("%d✓", passed))
	failPart := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(fmt.Sprintf("%d✗", failed))
	slashTot := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render(fmt.Sprintf("/ %d", total))
	checks := "(" + passPart + " " + failPart + " " + slashTot + ")"

	return strings.Join([]string{mergeS, checks}, " ")
}
