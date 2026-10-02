package bench

import (
	"encoding/json"
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCompute(t *testing.T) {
	preds := []Prediction{
		{Type: "choice", Label: 0, P: []float64{0.95, 0.05}},   // right, confident
		{Type: "choice", Label: 1, P: []float64{0.92, 0.08}},   // wrong, confident
		{Type: "noul", Label: 1, P: []float64{0.4, 0.6}},       // right
		{Type: "score", Label: 2, P: []float64{0.1, 0.2, 0.7}}, // right; expected level 1.6
		{Type: "choice", Error: "boom"},
	}
	m := Compute(preds)
	if m.N != 4 || m.Errors != 1 {
		t.Fatalf("n=%d errors=%d", m.N, m.Errors)
	}
	if !near(m.Acc, 0.75) {
		t.Errorf("acc %v", m.Acc)
	}
	wantBrier := (0.05*0.05*2 + (0.92*0.92 + 0.92*0.92) + (0.4*0.4 + 0.4*0.4) + (0.01 + 0.04 + 0.09)) / 4
	if !near(m.Brier, wantBrier) {
		t.Errorf("brier %v, want %v", m.Brier, wantBrier)
	}
	if !near(m.ConfidentErrorRate, 0.25) || !near(m.CoverageAt09, 0.5) || m.AccAt09 == nil || !near(*m.AccAt09, 0.5) {
		t.Errorf("confidence metrics wrong: %+v", m)
	}
	if m.ScoreMAE == nil || !near(*m.ScoreMAE, 0.4) {
		t.Errorf("score MAE %v", m.ScoreMAE)
	}
	// Bins: 0.6 and 0.7 are right (|1-0.6|, |1-0.7|), 0.92 and 0.95 share a bin with accuracy 0.5.
	wantECE := 0.25*0.4 + 0.25*0.3 + 0.5*math.Abs(0.5-0.935)
	if !near(m.ECE, wantECE) {
		t.Errorf("ece %v, want %v", m.ECE, wantECE)
	}
}

func TestRobustnessComparesByKeyNotPosition(t *testing.T) {
	preds := []Prediction{
		{ID: "r1", Question: "q", Variant: "clean", Keys: []string{"a", "b"}, P: []float64{0.9, 0.1}},
		// Same options reversed; still picks "a", so no flip even though the index differs.
		{ID: "r1/p", Parent: "r1", Question: "q", Variant: "permuted", Keys: []string{"b", "a"}, P: []float64{0.2, 0.8}},
		{ID: "r2", Question: "q", Variant: "clean", Keys: []string{"a", "b"}, P: []float64{0.9, 0.1}},
		{ID: "r2/p", Parent: "r2", Question: "q", Variant: "permuted", Keys: []string{"b", "a"}, P: []float64{0.8, 0.2}},
	}
	r := ComputeRobustness(preds)
	if r.PermutedN != 2 || r.PermutedFlip == nil || !near(*r.PermutedFlip, 0.5) {
		t.Errorf("got %+v", r)
	}
}

func TestKeysAndRequest(t *testing.T) {
	var rec Record
	json.Unmarshal([]byte(`{"state":"s","questions":{"q":{"type":"choice","instructions":"?","criteria":{"z":null,"a":"x"},"label":"a","src":"t"}},"_meta":{"id":"1","variant":"clean"}}`), &rec)
	keys, label, err := rec.Questions["q"].Keys()
	if err != nil || len(keys) != 2 || keys[0] != "z" || label != 1 {
		t.Fatalf("keys %v label %d err %v (criteria order must be kept)", keys, label, err)
	}
	body, _ := rec.Request("m")
	var req map[string]any
	json.Unmarshal(body, &req)
	q := req["questions"].(map[string]any)["q"].(map[string]any)
	if _, leaked := q["label"]; leaked {
		t.Error("label must not be sent to the model")
	}
	if req["model"] != "m" {
		t.Error("model not set")
	}
}

func TestFromJevBench(t *testing.T) {
	noul := `{"id":"e1","family":"fact","state":"s","question":{"type":"noul","instructions":"?","criteria":{"false":"n","true":"y"}},"expected":"yes"}`
	r, err := fromJevBench([]byte(noul))
	if err != nil {
		t.Fatal(err)
	}
	keys, label, err := r.Questions["q"].Keys()
	if err != nil || label != 1 || keys[1] != "true" || r.Questions["q"].Task != "fact" || r.Meta.ID != "e1" {
		t.Errorf("noul: keys %v label %d err %v %+v", keys, label, err, r)
	}
	score := `{"id":"h1","family":"ordinal","state":{"a":1},"question":{"type":"score","instructions":"?","criteria":["x","y","z"]},"expected":2}`
	r, _ = fromJevBench([]byte(score))
	if _, label, err := r.Questions["q"].Keys(); err != nil || label != 2 {
		t.Errorf("score label %d err %v", label, err)
	}
	if _, err := fromJevBench([]byte(`{"id":"b","question":{"type":"noul"},"expected":"maybe"}`)); err == nil {
		t.Error("noul expected must be yes or no")
	}
}
