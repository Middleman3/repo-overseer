package tree

import (
	"path/filepath"
	"sort"
	"strings"
)

// Repo is a git repository under the scan root.
type Repo struct {
	Name string // final path segment
	Abs  string // absolute path to repo root
	Rel  string // path relative to scan root (slash-separated)
}

// Dir is a directory node in the scan tree.
type Dir struct {
	Name    string
	Rel     string // relative path from scan root; "" for root
	Subdirs []*Dir
	Repos   []Repo
}

// RowKind distinguishes folder rows from repo rows in the flattened view.
type RowKind int

const (
	RowFolder RowKind = iota
	RowRepo
)

// Row is one visible line in the tree (after applying expand state).
type Row struct {
	Kind  RowKind
	Depth int
	Rel   string // folder rel for RowFolder; repo Rel for RowRepo
	Abs   string // set for RowRepo only
	Label string // display name (folder or repo basename)
}

// Build constructs a directory tree from absolute repo paths and a scan root.
func Build(scanRoot string, absRepos []string) *Dir {
	root := &Dir{Name: ".", Rel: ""}
	for _, abs := range absRepos {
		rel, err := filepath.Rel(scanRoot, abs)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || rel == "" {
			root.Repos = append(root.Repos, Repo{Name: filepath.Base(abs), Abs: abs, Rel: "."})
			continue
		}
		parts := strings.Split(rel, "/")
		root.insert(parts, abs, rel)
	}
	root.sortRec()
	return root
}

func (d *Dir) insert(parts []string, abs, fullRel string) {
	if len(parts) == 1 {
		d.Repos = append(d.Repos, Repo{Name: parts[0], Abs: abs, Rel: fullRel})
		return
	}
	head := parts[0]
	subRel := head
	if d.Rel != "" {
		subRel = d.Rel + "/" + head
	}
	sub := d.getOrCreateSubdir(head, subRel)
	sub.insert(parts[1:], abs, fullRel)
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
	sort.Slice(d.Repos, func(i, j int) bool {
		return d.Repos[i].Name < d.Repos[j].Name
	})
	for _, s := range d.Subdirs {
		s.sortRec()
	}
}

// IsExpanded returns whether rel is expanded (default true when unset).
func IsExpanded(expanded map[string]bool, rel string) bool {
	if expanded == nil {
		return true
	}
	v, ok := expanded[rel]
	if !ok {
		return true
	}
	return v
}

// Flatten walks the tree in order: subdirs (each recursively if expanded), then repos at this level.
func Flatten(d *Dir, expanded map[string]bool, depth int, out *[]Row) {
	for _, sub := range d.Subdirs {
		*out = append(*out, Row{
			Kind:  RowFolder,
			Depth: depth,
			Rel:   sub.Rel,
			Label: sub.Name,
		})
		if IsExpanded(expanded, sub.Rel) {
			Flatten(sub, expanded, depth+1, out)
		}
	}
	for _, r := range d.Repos {
		*out = append(*out, Row{
			Kind:  RowRepo,
			Depth: depth,
			Rel:   r.Rel,
			Abs:   r.Abs,
			Label: r.Name,
		})
	}
}
