package scan

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// GitRoots returns canonical absolute paths of every git work tree under root
// (detects .git directories and .git submodule gitfiles).
func GitRoots(root string) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", root)
	}

	seen := map[string]struct{}{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if !d.IsDir() && name != ".git" {
			return nil
		}
		if d.IsDir() && name == ".git" {
			repo := filepath.Dir(path)
			abs, e := filepath.Abs(repo)
			if e != nil {
				return e
			}
			seen[abs] = struct{}{}
			return filepath.SkipDir
		}
		if !d.IsDir() && name == ".git" {
			repo := filepath.Dir(path)
			abs, e := filepath.Abs(repo)
			if e != nil {
				return e
			}
			seen[abs] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
