package branchtree

import (
	"testing"

	"nested-git-tui/internal/gitx"
)

func TestFlattenCollapse(t *testing.T) {
	us := []gitx.UnifiedBranch{
		{FullName: "a/x", Local: &gitx.Ref{Name: "x"}},
		{FullName: "a/y", Local: &gitx.Ref{Name: "y"}},
	}
	root := Build(us)
	var rows []Row
	Flatten(root, 0, func(rel string) bool {
		return rel != "a" // collapse folder "a"
	}, &rows)
	for _, r := range rows {
		if r.Kind == RowBranch && (r.Label == "x" || r.Label == "y") {
			t.Fatalf("collapsed folder should hide branches, got branch row %+v", r)
		}
	}
}
