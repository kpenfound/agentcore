package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kpenfound/agentcore/agent/agenttest"
)

// sizedRun runs profile through a Runner on the fake sbx and returns the
// arguments of the `sbx create` it recorded (nil when none was made), the
// directory of the fake, and the error.
func sizedRun(t *testing.T, r *testRunner, p Profile) (create []string, err error) {
	t.Helper()
	_, err = r.Run(context.Background(), Request{Name: "sized", Profile: p, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if b, rerr := os.ReadFile(filepath.Join(filepath.Dir(r.SbxBin), "sbx-create.txt")); rerr == nil {
		create = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	return create, err
}

func sizedRunner(t *testing.T) *testRunner {
	t.Helper()
	r := newRunner(t, fakeClaude(t, `echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeTaskServer(t)
	r.StateDir = t.TempDir()
	return r
}

// sbxCalls are the create, policy and rm calls the fake recorded, none when
// the fake was not called for them.
func sbxCalls(t *testing.T, sbx string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(sbx), "sbx-calls.txt"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// optionsBeforeAgent are the arguments of `sbx create` between the fixed
// leading ones (create --no-progress --name <name> --skills off) and the
// agent's name.
func optionsBeforeAgent(t *testing.T, create []string, agent string) []string {
	t.Helper()
	i := slices.Index(create, agent)
	if len(create) < 6 || i < 6 {
		t.Fatalf("sbx create %v has no agent %s after its options", create, agent)
	}
	return create[6:i]
}

// A profile that leaves the sizing unset creates its sandbox with the
// arguments it always had, with a template and without one.
func TestSandboxSizingUnsetLeavesCreateUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		want        []string
	}{
		{"template", "ghcr.io/acme/task:1", []string{"--template", "ghcr.io/acme/task:1"}},
		{"no template", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sizedRunner(t)
			create, err := sizedRun(t, r, Profile{Name: "builder", Sandbox: SandboxSbx, SandboxImage: tc.image})
			if err != nil {
				t.Fatal(err)
			}
			if len(create) < 7 || create[0] != "create" || create[1] != "--no-progress" || create[2] != "--name" || create[4] != "--skills" || create[5] != "off" {
				t.Fatalf("sbx create args: %v", create)
			}
			if got := optionsBeforeAgent(t, create, "claude"); !slices.Equal(got, tc.want) {
				t.Errorf("options between --skills off and the agent: %v, want %v", got, tc.want)
			}
		})
	}
}

// Each setting reaches `sbx create` alone, among its options and before the
// agent's name.
func TestSandboxSizingReachesCreate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cpus   int
		memory string
		image  string
		want   []string
	}{
		{"both", 2, "4g", "", []string{"--cpus", "2", "--memory", "4g"}},
		{"both with a template", 4, "512m", "acme/t:1", []string{"--template", "acme/t:1", "--cpus", "4", "--memory", "512m"}},
		{"cpus only", 3, "", "", []string{"--cpus", "3"}},
		{"memory only", 0, "8g", "", []string{"--memory", "8g"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sizedRunner(t)
			create, err := sizedRun(t, r, Profile{Name: "builder", Sandbox: SandboxSbx, SandboxImage: tc.image, SandboxCPUs: tc.cpus, SandboxMemory: tc.memory})
			if err != nil {
				t.Fatal(err)
			}
			if got := optionsBeforeAgent(t, create, "claude"); !slices.Equal(got, tc.want) {
				t.Errorf("options between --skills off and the agent: %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSandboxSizingValidation(t *testing.T) {
	// Unset is accepted in every sandbox mode.
	for _, sandbox := range []string{"", SandboxNone, SandboxClaude, SandboxContainer, SandboxSbx} {
		p := Profile{Sandbox: sandbox, SandboxImage: "acme/t:1"}
		if err := p.Validate(); err != nil {
			t.Errorf("sandbox %q with no sizing: %v", sandbox, err)
		}
	}
	for _, p := range []Profile{
		{Sandbox: SandboxSbx, SandboxCPUs: 1},
		{Sandbox: SandboxSbx, SandboxMemory: "512m"},
		{Sandbox: SandboxSbx, SandboxCPUs: 16, SandboxMemory: "32g"},
		{Sandbox: SandboxSbx, SandboxMemory: "1000m"},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}

	// A negative count and a malformed limit are refused, naming the field.
	err := Profile{Sandbox: SandboxSbx, SandboxCPUs: -1}.Validate()
	if err == nil || !strings.Contains(err.Error(), "SandboxCPUs") {
		t.Errorf("negative SandboxCPUs: %v", err)
	}
	for _, memory := range []string{"512", "4", "4G", "512M", "4k", "4gb", "4t", "4 g", " 4g", "4g ", "4\ng", "-4g", "+4g", "0g", "0m", "04g", "4.5g", "g", "m", "1e3m", "4g;ls", "4g&&id", "$(id)", "`id`", "4g|cat", "--cpus", "4g\"", "4g'", "4g\\", "4g#", "4g>x", "4g*", "٤g", "4g\x00"} {
		err := Profile{Sandbox: SandboxSbx, SandboxMemory: memory}.Validate()
		if err == nil || !strings.Contains(err.Error(), "SandboxMemory") {
			t.Errorf("SandboxMemory %q: %v", memory, err)
		}
	}

	// Neither is given to a sandbox that is not sbx.
	for _, sandbox := range []string{"", SandboxNone, SandboxClaude, SandboxContainer} {
		base := Profile{Sandbox: sandbox, SandboxImage: "acme/t:1"}
		cpus, memory := base, base
		cpus.SandboxCPUs, memory.SandboxMemory = 2, "4g"
		if err := cpus.Validate(); err == nil || !strings.Contains(err.Error(), "SandboxCPUs") {
			t.Errorf("sandbox %q with SandboxCPUs: %v", sandbox, err)
		}
		if err := memory.Validate(); err == nil || !strings.Contains(err.Error(), "SandboxMemory") {
			t.Errorf("sandbox %q with SandboxMemory: %v", sandbox, err)
		}
		// A negative count names the field there too.
		cpus.SandboxCPUs = -1
		if err := cpus.Validate(); err == nil || !strings.Contains(err.Error(), "SandboxCPUs") {
			t.Errorf("sandbox %q with a negative SandboxCPUs: %v", sandbox, err)
		}
	}

	// The exported checks are the ones Validate applies.
	if CheckSandboxCPUs(0) != nil || CheckSandboxCPUs(2) != nil || CheckSandboxCPUs(-2) == nil {
		t.Error("CheckSandboxCPUs")
	}
	if CheckSandboxMemory("") != nil || CheckSandboxMemory("2g") != nil || CheckSandboxMemory("2") == nil {
		t.Error("CheckSandboxMemory")
	}

	// A fallback's sizing does not widen the primary's validation.
	p := Profile{Sandbox: SandboxNone, Fallback: &Profile{Sandbox: SandboxNone, SandboxCPUs: -3, SandboxMemory: "x"}}
	if err := p.Validate(); err != nil {
		t.Errorf("a primary is refused for its fallback's sizing: %v", err)
	}
}

// A profile that is refused never reaches sbx.
func TestSandboxSizingRefusedProfileRunsNoSbx(t *testing.T) {
	for _, p := range []Profile{
		{Name: "a", Sandbox: SandboxSbx, SandboxCPUs: -1},
		{Name: "b", Sandbox: SandboxSbx, SandboxMemory: "4"},
		{Name: "c", Sandbox: SandboxSbx, SandboxMemory: "4g; rm -rf /"},
	} {
		r := sizedRunner(t)
		create, err := sizedRun(t, r, p)
		if err == nil || !strings.Contains(err.Error(), "Sandbox") {
			t.Errorf("%s: %v", p.Name, err)
		}
		if create != nil || sbxCalls(t, r.SbxBin) != nil {
			t.Errorf("%s: sbx was called: create %v, calls %v", p.Name, create, sbxCalls(t, r.SbxBin))
		}
		for _, f := range []string{"sbx-setup.txt", "sbx-policy.txt", "sbx-rm.txt", "sbx-probe.txt"} {
			if _, err := os.Stat(filepath.Join(filepath.Dir(r.SbxBin), f)); err == nil {
				t.Errorf("%s: the fake recorded %s", p.Name, f)
			}
		}
	}
}

// A turn through the sbx enforcer creates its sandbox with the profile's
// sizing, and a refused profile is refused before any sbx call after Prepare.
func TestSbxEnforcerAppliesTheSizing(t *testing.T) {
	run := func(t *testing.T, template string, mutate func(*Profile)) (create []string, calls []string, err error) {
		t.Helper()
		l := newSbxLayout(t)
		server := newHostListener(t, "host-secret")
		sbx := agenttest.Sbx(t, "RUN_DIR")
		r := Runner{ClaudeBin: hostAgent(t, "claude", sbx, server.port, claudeResult), SbxBin: sbx}
		s, perr := NewSbx(r, "", template).Prepare(context.Background(), l.grants([]string{"Read"}, server.port))
		if perr != nil {
			t.Fatal(perr)
		}
		defer func() { _ = s.Release(context.Background()) }()
		// Prepare ran `sbx version` and nothing else; the baseline is here.
		if got := sbxCalls(t, sbx); got != nil {
			t.Fatalf("Prepare made sbx calls: %v", got)
		}
		req := l.turn("http://127.0.0.1:" + strconv.Itoa(server.port) + "/mcp")
		mutate(&req.Profile)
		_, err = s.Run(context.Background(), req)
		if b, rerr := os.ReadFile(filepath.Join(filepath.Dir(sbx), "sbx-create.txt")); rerr == nil {
			create = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		}
		return create, sbxCalls(t, sbx), err
	}

	t.Run("sized", func(t *testing.T) {
		create, _, err := run(t, "acme/t:1", func(p *Profile) { p.SandboxCPUs, p.SandboxMemory = 2, "4g" })
		if err != nil {
			t.Fatal(err)
		}
		if got, want := optionsBeforeAgent(t, create, "claude"), []string{"--template", "acme/t:1", "--cpus", "2", "--memory", "4g"}; !slices.Equal(got, want) {
			t.Errorf("options: %v, want %v", got, want)
		}
	})
	t.Run("cpus only", func(t *testing.T) {
		create, _, err := run(t, "", func(p *Profile) { p.SandboxCPUs = 1 })
		if err != nil {
			t.Fatal(err)
		}
		if got, want := optionsBeforeAgent(t, create, "claude"), []string{"--cpus", "1"}; !slices.Equal(got, want) {
			t.Errorf("options: %v, want %v", got, want)
		}
	})
	t.Run("unset", func(t *testing.T) {
		create, _, err := run(t, "", func(*Profile) {})
		if err != nil {
			t.Fatal(err)
		}
		if got := optionsBeforeAgent(t, create, "claude"); len(got) != 0 {
			t.Errorf("options: %v, want none", got)
		}
	})
	for name, mutate := range map[string]func(*Profile){
		"negative cpus":  func(p *Profile) { p.SandboxCPUs = -2 },
		"memory no unit": func(p *Profile) { p.SandboxMemory = "512" },
		"memory metas":   func(p *Profile) { p.SandboxMemory = "1g $(id)" },
	} {
		t.Run("refused "+name, func(t *testing.T) {
			create, calls, err := run(t, "", mutate)
			if err == nil || !strings.Contains(err.Error(), "Sandbox") {
				t.Fatalf("run: %v", err)
			}
			if create != nil || calls != nil {
				t.Errorf("sbx was called after Prepare: create %v, calls %v", create, calls)
			}
		})
	}
}

// A caller that runs a profile of a Fallback chain gets that profile's own
// sizing, validated and applied, and never the primary's.
func TestSandboxSizingFallbackCarriesItsOwn(t *testing.T) {
	fallback := &Profile{Name: "fallback", Sandbox: SandboxSbx, SandboxCPUs: 1, SandboxMemory: "1g"}
	primary := Profile{Name: "primary", Sandbox: SandboxSbx, SandboxCPUs: 4, SandboxMemory: "8g", Fallback: fallback}
	cpusOnly := &Profile{Name: "fb-cpus", Sandbox: SandboxSbx, SandboxCPUs: 2}
	memoryOnly := &Profile{Name: "fb-memory", Sandbox: SandboxSbx, SandboxMemory: "2g"}
	unsized := &Profile{Name: "fb-unset", Sandbox: SandboxSbx}

	for _, tc := range []struct {
		name string
		p    Profile
		want []string
	}{
		{"primary", primary, []string{"--cpus", "4", "--memory", "8g"}},
		{"fallback", *primary.Fallback, []string{"--cpus", "1", "--memory", "1g"}},
		{"cpus-only fallback", *cpusOnly, []string{"--cpus", "2"}},
		{"memory-only fallback", *memoryOnly, []string{"--memory", "2g"}},
		{"unset fallback", *unsized, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := sizedRunner(t)
			create, err := sizedRun(t, r, tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if got := optionsBeforeAgent(t, create, "claude"); !slices.Equal(got, tc.want) {
				t.Errorf("options: %v, want %v", got, tc.want)
			}
		})
	}

	// An invalid fallback is refused when it is run, before sbx create, and
	// its valid primary still runs.
	bad := &Profile{Name: "bad", Sandbox: SandboxSbx, SandboxCPUs: 2, SandboxMemory: "2"}
	valid := Profile{Name: "primary", Sandbox: SandboxSbx, SandboxCPUs: 4, Fallback: bad}
	if err := valid.Validate(); err != nil {
		t.Fatalf("the primary is refused for its fallback: %v", err)
	}
	r := sizedRunner(t)
	if create, err := sizedRun(t, r, valid); err != nil {
		t.Fatalf("primary: %v", err)
	} else if got := optionsBeforeAgent(t, create, "claude"); !slices.Equal(got, []string{"--cpus", "4"}) {
		t.Errorf("primary options: %v", got)
	}
	r = sizedRunner(t)
	create, err := sizedRun(t, r, *valid.Fallback)
	if err == nil || !strings.Contains(err.Error(), "SandboxMemory") {
		t.Fatalf("the invalid fallback ran: %v", err)
	}
	if create != nil || sbxCalls(t, r.SbxBin) != nil {
		t.Errorf("sbx was called for the invalid fallback: %v %v", create, sbxCalls(t, r.SbxBin))
	}
}
