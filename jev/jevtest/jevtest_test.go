package jevtest_test

import (
	"testing"

	"github.com/kpenfound/agentcore/jev/jevtest"
	"github.com/kpenfound/agentcore/jev/systemone"
)

func TestScoreKeysProbabilitiesByLevel(t *testing.T) {
	a := jevtest.Score(1.2, 0.7, 0.1, 0.6, 0.3)
	if a.Kind != systemone.KindScore || a.Score != 1.2 || a.Confidence != 0.7 {
		t.Fatalf("answer = %+v", a)
	}
	want := map[string]float64{"0": 0.1, "1": 0.6, "2": 0.3}
	if len(a.Probabilities) != len(want) {
		t.Fatalf("probabilities = %v", a.Probabilities)
	}
	for k, p := range want {
		if a.Probabilities[k] != p {
			t.Fatalf("probabilities = %v", a.Probabilities)
		}
	}
}
