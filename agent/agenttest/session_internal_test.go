package agenttest

import (
	"bufio"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"
)

// An app-server process used only for preflight must not take the next turn.
// Wire details stay inside the fixture package; caller tests use only Turns.
func TestSessionAppServerPreflight(t *testing.T) {
	f := Session(t, "codex", Turn{ResultText: "first"}, Turn{ResultText: "second"})
	cmd := exec.Command(f.Bin, "app-server", "-c", `tools.update_plan.enabled=false`)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	for i, method := range []string{"initialize", "config/read"} {
		request := map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": method}
		if err := json.NewEncoder(stdin).Encode(request); err != nil {
			t.Fatal(err)
		}
		if !scanner.Scan() {
			t.Fatal("probe ended before its answer")
		}
		var answer struct {
			ID     int `json:"id"`
			Result struct {
				Origins map[string]struct {
					Name struct {
						Type string `json:"type"`
					} `json:"name"`
				} `json:"origins"`
			} `json:"result"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		if answer.ID != i+1 {
			t.Fatalf("answer id: %d", answer.ID)
		}
		if method == "config/read" && answer.Result.Origins["tools.update_plan.enabled"].Name.Type != "sessionFlags" {
			t.Fatalf("origins: %+v", answer)
		}
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	rec := f.Record(t)
	if rec.Starts != 0 || len(rec.Probes) != 1 || !slices.Equal(rec.Probes[0].Methods, []string{"initialize", "config/read"}) {
		t.Fatalf("preflight consumed a turn: %+v", rec)
	}
}
