package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent/agenttest"
)

// gitInit makes dir an empty repository the way a caller does, with git's
// own configuration files left out.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "--quiet", "--template=", dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// A sandbox session admits a writable mount marked as an empty repository,
// reports the mark in its policy and keeps VCS denied. The mark is checked
// on every turn: once the repository holds an object, the next turn is
// refused before a sandbox is created.
func TestASandboxSessionAdmitsAMarkedEmptyRepository(t *testing.T) {
	l := newSbxLayout(t)
	repo := filepath.Join(filepath.Dir(l.work), "repo")
	gitInit(t, repo)
	sbx := agenttest.Sbx(t, "RUN_DIR")
	engine := filepath.Dir(sbx)
	r := Runner{ClaudeBin: hostAgent(t, "claude", sbx, 0, claudeResult), SbxBin: sbx}

	g := l.grants([]string{ToolsAll}, 8080)
	g.Mounts = append(g.Mounts, Mount{Path: repo, Access: ReadWrite})
	if _, err := NewSbx(r, "", "").Prepare(context.Background(), g); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("an unmarked writable repository: %v, want ErrNotGranted", err)
	}
	g.Mounts[len(g.Mounts)-1].EmptyRepository = true
	s, err := NewSbx(r, "", "").Prepare(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Release(context.Background()) }()

	p := s.Policy()
	want := []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}, {Path: repo, Access: ReadWrite, EmptyRepository: true}}
	if !slices.Equal(p.Mounts, want) {
		t.Errorf("policy mounts %+v, want %+v", p.Mounts, want)
	}
	if p.VCS || !slices.Equal(p.DeniedExecutables, VCSExecutables) {
		t.Errorf("policy VCS %v, denied executables %v: want VCS denied", p.VCS, p.DeniedExecutables)
	}
	if !p.Writes(filepath.Join(repo, ".git", "config")) {
		t.Error("the policy says the marked repository's .git is not writable")
	}

	res, err := s.Run(context.Background(), l.turn("http://127.0.0.1:8080/mcp"))
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	create := lines(t, filepath.Join(engine, "sbx-create.txt"))
	if !slices.Contains(create, repo) {
		t.Errorf("sbx create %v does not give the repository writable", create)
	}

	writeFile(t, filepath.Join(repo, ".git", "objects", "ab", "cdef"), "x")
	if _, err := s.Run(context.Background(), l.turn("http://127.0.0.1:8080/mcp")); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("a turn after the repository holds an object: %v, want ErrNotGranted", err)
	}
	creates := 0
	for _, line := range lines(t, filepath.Join(engine, "sbx-calls.txt")) {
		if strings.HasPrefix(line, "create ") {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("sbx create ran %d times, want once: the refused turn created a sandbox", creates)
	}
}

// A marked mount is admitted only where it is the repository git init
// leaves; everything else stays refused, the way it is without the mark.
func TestAMarkedMountMustBeAnEmptyRepository(t *testing.T) {
	for name, tc := range map[string]struct {
		// mutate changes the empty repository at repo and returns the path
		// to mount, or "" for repo.
		mutate func(t *testing.T, repo string) string
		// unmarked leaves the mount unmarked.
		unmarked bool
	}{
		"an unmarked mount": {mutate: func(*testing.T, string) string { return "" }, unmarked: true},
		"an object": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "objects", "ab", "cdef"), "x")
			return ""
		}},
		"an object pack": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "objects", "pack", "pack-1.pack"), "x")
			return ""
		}},
		"alternates": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), "/elsewhere/objects\n")
			return ""
		}},
		"a ref": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "refs", "heads", "main"), strings.Repeat("a", 40)+"\n")
			return ""
		}},
		"packed-refs": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "packed-refs"), "# pack-refs with: peeled\n")
			return ""
		}},
		"a hook": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "hooks", "post-checkout"), "#!/bin/sh\n")
			return ""
		}},
		"an empty hooks directory": {mutate: func(t *testing.T, repo string) string {
			if err := os.Mkdir(filepath.Join(repo, ".git", "hooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"a symbolic link under refs": {mutate: func(t *testing.T, repo string) string {
			if err := os.Symlink(t.TempDir(), filepath.Join(repo, ".git", "refs", "heads", "x")); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"objects a symbolic link": {mutate: func(t *testing.T, repo string) string {
			objects := filepath.Join(repo, ".git", "objects")
			if err := os.RemoveAll(objects); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), objects); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"HEAD a symbolic link": {mutate: func(t *testing.T, repo string) string {
			head := filepath.Join(repo, ".git", "HEAD")
			other := filepath.Join(t.TempDir(), "HEAD")
			writeFile(t, other, "ref: refs/heads/main\n")
			if err := os.Remove(head); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, head); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"a detached HEAD": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "HEAD"), strings.Repeat("a", 40)+"\n")
			return ""
		}},
		"a remote": {mutate: func(t *testing.T, repo string) string {
			appendFile(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = https://example.com/x.git\n")
			return ""
		}},
		"an include": {mutate: func(t *testing.T, repo string) string {
			appendFile(t, filepath.Join(repo, ".git", "config"), "[include]\n\tpath = /elsewhere/config\n")
			return ""
		}},
		"an includeIf": {mutate: func(t *testing.T, repo string) string {
			appendFile(t, filepath.Join(repo, ".git", "config"), "[includeIf \"gitdir:/\"]\n\tpath = /elsewhere/config\n")
			return ""
		}},
		"a hooks path": {mutate: func(t *testing.T, repo string) string {
			appendFile(t, filepath.Join(repo, ".git", "config"), "[core]\n\thooksPath = /elsewhere/hooks\n")
			return ""
		}},
		"a hooks path in other case on the header's line": {mutate: func(t *testing.T, repo string) string {
			appendFile(t, filepath.Join(repo, ".git", "config"), "[CORE] HOOKSPATH = /elsewhere/hooks\n")
			return ""
		}},
		"a remote behind a byte-order mark": {mutate: func(t *testing.T, repo string) string {
			writeFile(t, filepath.Join(repo, ".git", "config"), "\xef\xbb\xbf[remote \"origin\"]\n\turl = x\n")
			return ""
		}},
		"a .git file": {mutate: func(t *testing.T, repo string) string {
			if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(repo, ".git"), "gitdir: /elsewhere\n")
			return ""
		}},
		".git a symbolic link to an empty repository": {mutate: func(t *testing.T, repo string) string {
			other := filepath.Join(t.TempDir(), "other")
			gitInit(t, other)
			if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(other, ".git"), filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"no .git": {mutate: func(t *testing.T, repo string) string {
			if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		".hg beside .git":  {mutate: vcsBeside(".hg")},
		".jj beside .git":  {mutate: vcsBeside(".jj")},
		".svn beside .git": {mutate: vcsBeside(".svn")},
		"a mount inside .git": {mutate: func(_ *testing.T, repo string) string {
			return filepath.Join(repo, ".git", "refs")
		}},
		"the .git directory itself": {mutate: func(_ *testing.T, repo string) string {
			return filepath.Join(repo, ".git")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			work := realTemp(t)
			repo := filepath.Join(realTemp(t), "repo")
			gitInit(t, repo)
			g := &Grants{Env: []string{"PATH"}, Tools: []string{ToolsAll}, Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: repo, Access: ReadWrite, EmptyRepository: true}}}
			req := Request{Workspace: fakeWorkspace{dir: work}, SessionDir: work, Grants: g, Profile: Profile{Sandbox: SandboxSbx}}
			if _, err := (SandboxBoundary{}).Verify(req); err != nil {
				t.Fatalf("the empty repository: %v", err)
			}
			if p := tc.mutate(t, repo); p != "" {
				g.Mounts[1].Path = p
			}
			g.Mounts[1].EmptyRepository = !tc.unmarked
			if _, err := (SandboxBoundary{}).Verify(req); !errors.Is(err, ErrNotGranted) {
				t.Errorf("turn: %v, want ErrNotGranted", err)
			}
			if _, err := NewPolicy(SandboxSbx, *g); !errors.Is(err, ErrNotGranted) {
				t.Errorf("policy: %v, want ErrNotGranted", err)
			}
		})
	}
}

func vcsBeside(dir string) func(*testing.T, string) string {
	return func(t *testing.T, repo string) string {
		if err := os.Mkdir(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		return ""
	}
}

// Every sandbox but sbx refuses a marked mount, read-only or writable, when
// it is prepared and when a turn is verified, even with VCS granted.
func TestAMarkedMountIsRefusedOutsideSbx(t *testing.T) {
	work := realTemp(t)
	repo := filepath.Join(realTemp(t), "repo")
	gitInit(t, repo)
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "empty repository") {
			t.Errorf("%s: %v, want ErrUnsupported for the marked mount", what, err)
		}
	}
	for _, access := range []Access{ReadOnly, ReadWrite} {
		g := Grants{Env: []string{"PATH"}, Tools: []string{ToolsAll}, VCS: true,
			Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: repo, Access: access, EmptyRepository: true}}}
		for _, kind := range []string{SandboxNone, SandboxClaude, SandboxContainer} {
			_, err := NewPolicy(kind, g)
			refused(kind+" "+string(access)+" policy", err)
			req := Request{Workspace: fakeWorkspace{dir: work}, SessionDir: work, Grants: &g, Profile: Profile{Sandbox: kind, VCSAccess: true}}
			_, err = verifyCommon(req)
			refused(kind+" "+string(access)+" turn", err)
			if hostKind(kind) {
				_, err = HostBoundary{}.Verify(req)
				refused(kind+" "+string(access)+" host boundary", err)
			} else {
				_, err = ContainerBoundary{}.Verify(req)
				refused(kind+" "+string(access)+" container boundary", err)
			}
		}
		for name, e := range map[string]Enforcer{
			"host none":   NewHostNone(Runner{}),
			"host claude": NewHostClaude(Runner{}),
			"container":   NewContainer(Runner{}, "image"),
		} {
			s, err := e.Prepare(context.Background(), g)
			if s != nil {
				t.Errorf("%s %s: prepared %v", name, access, s)
			}
			refused(name+" "+string(access)+" prepare", err)
		}
	}
}
