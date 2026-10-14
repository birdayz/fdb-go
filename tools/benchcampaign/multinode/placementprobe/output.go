package placementprobe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// InsideGitWorktree reports whether path (existing or not) lies inside a
// directory holding .git, after resolving symlinks of its parent directory.
func InsideGitWorktree(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(abs)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return true, nil
		}
		if filepath.Dir(p) == p {
			return false, nil
		}
	}
}

// WriteJSONExclusive writes v as indented JSON to a new file outside any Git
// work tree; it never overwrites.
func WriteJSONExclusive(path string, v any) error {
	inside, err := InsideGitWorktree(path)
	if err != nil {
		return err
	}
	if inside {
		return fmt.Errorf("%s is inside a Git work tree; campaign receipts live outside the repository", path)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
