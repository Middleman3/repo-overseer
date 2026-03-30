package branchtree

import (
	"sort"
	"strings"

	"nested-git-tui/internal/ghpr"
	"nested-git-tui/internal/gitx"
)

// Dir groups branches by / path segments (same idea as repo directory tree).
type Dir struct {
	Name    string
	Rel     string // relative path from root ("" for root)
	Subdirs []*Dir
	Leaves  []*Leaf // branches whose full name ends at this level
}

// Leaf is a branch name segment with its unified data.
type Leaf struct {
	Name string
	Full string
	U    *gitx.UnifiedBranch
	PRs  []ghpr.PR // open PRs whose head matches this branch (by value)
}

// RowKind is folder vs branch vs PR line in flattened output.
type RowKind int

const (
	RowFolder RowKind = iota
	RowBranch
	RowPR
	RowTag // lightweight tag (e.g. from archive); not a branch row
)

// Row is one visible line in the branch tree preview.
type Row struct {
	Kind    RowKind
	Depth   int
	Rel     string // folder path key
	Label   string
	U       *gitx.UnifiedBranch // only for RowBranch
	PR      *ghpr.PR            // only for RowPR
	TagName string              // only for RowTag (full tag name)
}

// Build constructs a tree from unified branches (FullName uses / for nesting).
func Build(us []gitx.UnifiedBranch) *Dir {
	root := &Dir{Name: ".", Rel: ""}
	for i := range us {
		u := &us[i]
		parts := strings.Split(u.FullName, "/")
		root.insert(parts, u)
	}
	root.sortRec()
	return root
}

func (d *Dir) insert(parts []string, u *gitx.UnifiedBranch) {
	if len(parts) == 1 {
		d.Leaves = append(d.Leaves, &Leaf{Name: parts[0], Full: u.FullName, U: u})
		return
	}
	head := parts[0]
	subRel := head
	if d.Rel != "" {
		subRel = d.Rel + "/" + head
	}
	sub := d.getOrCreateSubdir(head, subRel)
	sub.insert(parts[1:], u)
}

func (d *Dir) getOrCreateSubdir(name, rel string) *Dir {
	for _, s := range d.Subdirs {
		if s.Name == name {
			return s
		}
	}
	nd := &Dir{Name: name, Rel: rel}
	d.Subdirs = append(d.Subdirs, nd)
	return nd
}

func (d *Dir) sortRec() {
	sort.Slice(d.Subdirs, func(i, j int) bool {
		return d.Subdirs[i].Name < d.Subdirs[j].Name
	})
	sort.Slice(d.Leaves, func(i, j int) bool {
		return d.Leaves[i].Name < d.Leaves[j].Name
	})
	for _, s := range d.Subdirs {
		s.sortRec()
	}
}

// AssignPRs attaches open PRs to leaves by head branch name; returns PRs with no matching leaf.
func AssignPRs(d *Dir, prs []ghpr.PR) []ghpr.PR {
	var unmatched []ghpr.PR
	for i := range prs {
		pr := prs[i]
		name := ghpr.HeadBranchLocalName(pr.HeadRefName)
		leaf := d.findLeaf(name)
		if leaf != nil {
			leaf.PRs = append(leaf.PRs, pr)
		} else {
			unmatched = append(unmatched, pr)
		}
	}
	d.sortPRs()
	sort.Slice(unmatched, func(i, j int) bool { return unmatched[i].Number < unmatched[j].Number })
	return unmatched
}

func (d *Dir) findLeaf(fullBranchName string) *Leaf {
	parts := strings.Split(fullBranchName, "/")
	return d.findLeafParts(parts, fullBranchName)
}

func (d *Dir) findLeafParts(parts []string, full string) *Leaf {
	if len(parts) == 0 {
		return nil
	}
	if len(parts) == 1 {
		for _, L := range d.Leaves {
			if L.Full == full {
				return L
			}
		}
		return nil
	}
	head := parts[0]
	for _, sub := range d.Subdirs {
		if sub.Name == head {
			return sub.findLeafParts(parts[1:], full)
		}
	}
	return nil
}

func (d *Dir) sortPRs() {
	for _, L := range d.Leaves {
		sort.Slice(L.PRs, func(i, j int) bool { return L.PRs[i].Number < L.PRs[j].Number })
	}
	for _, sub := range d.Subdirs {
		sub.sortPRs()
	}
}

// Flatten walks folders then branches (depth-first). When a leaf has open PRs, PR rows
// replace the branch line at the same depth; otherwise the branch row is shown.
// isExpanded(rel) returns whether folder rel should show its children; if nil, all folders are expanded.
func Flatten(d *Dir, depth int, isExpanded func(string) bool, out *[]Row) {
	if isExpanded == nil {
		isExpanded = func(string) bool { return true }
	}
	for _, sub := range d.Subdirs {
		*out = append(*out, Row{
			Kind:  RowFolder,
			Depth: depth,
			Rel:   sub.Rel,
			Label: sub.Name,
		})
		if isExpanded(sub.Rel) {
			Flatten(sub, depth+1, isExpanded, out)
		}
	}
	for _, leaf := range d.Leaves {
		if len(leaf.PRs) > 0 {
			// Open PRs replace the branch line (same depth as the branch would use).
			for i := range leaf.PRs {
				pr := &leaf.PRs[i]
				*out = append(*out, Row{
					Kind:  RowPR,
					Depth: depth,
					Label: leaf.Name,
					U:     leaf.U,
					PR:    pr,
				})
			}
			continue
		}
		*out = append(*out, Row{
			Kind:  RowBranch,
			Depth: depth,
			Label: leaf.Name,
			U:     leaf.U,
		})
	}
}
