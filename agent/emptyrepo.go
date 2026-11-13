package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// emptyRepository returns why path is not a mount Mount.EmptyRepository may
// mark, or nil when it is one: a directory outside all VCS metadata whose
// only VCS metadata is a ".git" directory at its top holding what
// "git init --quiet --template=" leaves and nothing else. That is HEAD, a
// symbolic reference to a branch under refs/heads/; config, with no
// [remote], [include] or [includeIf] section and no core.hooksPath; and the
// directories objects and refs, which hold directories alone. Every entry
// is a regular file or a directory: a symbolic link anywhere is refused,
// and so is any other entry, packed-refs, hooks, an index or reftable
// among them.
func emptyRepository(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		if slices.Contains(vcsDirs, filepath.Base(p)) {
			return fmt.Errorf("it lies inside VCS metadata %s", p)
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	for _, d := range vcsDirs {
		if d == ".git" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(path, d)); err == nil {
			return fmt.Errorf("it holds VCS metadata %s", filepath.Join(path, d))
		}
	}
	git := filepath.Join(path, ".git")
	info, err := os.Lstat(git)
	if err != nil {
		return fmt.Errorf("it has no .git directory: %v", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", git)
	}
	entries, err := os.ReadDir(git)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(git, e.Name())
		switch e.Name() {
		case "HEAD":
			err = checkRepoFile(p, e, checkRepoHEAD)
		case "config":
			err = checkRepoFile(p, e, checkRepoConfig)
		case "objects", "refs":
			err = onlyDirectories(p)
		default:
			err = fmt.Errorf("%s holds %s, which git init does not leave", git, e.Name())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// maxRepoFile bounds what is read of HEAD or config.
const maxRepoFile = 64 << 10

func checkRepoFile(path string, e fs.DirEntry, check func([]byte) error) error {
	if !e.Type().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxRepoFile+1))
	if err != nil {
		return err
	}
	if len(b) > maxRepoFile {
		return fmt.Errorf("%s is larger than git init leaves it", path)
	}
	if err := check(b); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// checkRepoHEAD accepts a HEAD that names a branch and nothing else.
func checkRepoHEAD(b []byte) error {
	ref, ok := strings.CutPrefix(strings.TrimSuffix(string(b), "\n"), "ref: refs/heads/")
	if !ok || ref == "" || strings.ContainsAny(ref, "\n\r") {
		return errors.New("not a symbolic reference to a branch")
	}
	return nil
}

// checkRepoConfig refuses a config with a remote, an include or a hooks
// path, and one it cannot read line by line: every line is blank, a
// comment, one or more section headers optionally followed by a variable,
// or a variable. Names are compared the way git compares them, ignoring
// case.
func checkRepoConfig(b []byte) error {
	section := ""
	for i, line := range strings.Split(strings.TrimPrefix(string(b), "\xef\xbb\xbf"), "\n") {
		line = strings.TrimSpace(line)
		for strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				return fmt.Errorf("line %d: a section header without its ]", i+1)
			}
			header := line[1:end]
			if n := strings.IndexAny(header, " \t\"."); n >= 0 {
				header = header[:n]
			}
			section = strings.ToLower(header)
			switch section {
			case "remote", "include", "includeif":
				return fmt.Errorf("line %d: a [%s] section", i+1, section)
			}
			line = strings.TrimSpace(line[end+1:])
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key := line
		if n := strings.IndexAny(line, "= \t"); n >= 0 {
			key = line[:n]
		}
		if !configName(key) {
			return fmt.Errorf("line %d: not a section header, a comment or a variable", i+1)
		}
		if section == "core" && strings.EqualFold(key, "hooksPath") {
			return fmt.Errorf("line %d: core.hooksPath", i+1)
		}
	}
	return nil
}

// configName reports whether s is a git config variable name: a letter,
// then letters, digits and "-".
func configName(s string) bool {
	for i, r := range s {
		letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !letter && (i == 0 || (r != '-' && (r < '0' || r > '9'))) {
			return false
		}
	}
	return s != ""
}

// onlyDirectories refuses a tree that holds anything but directories: an
// object, a ref, or a symbolic link.
func onlyDirectories(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return fmt.Errorf("%s holds %s, which an empty repository does not", filepath.Dir(root), p)
		}
		return nil
	})
}
