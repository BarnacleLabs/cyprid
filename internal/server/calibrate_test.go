package server

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"

	"cyprid/internal/bench"
	"cyprid/internal/recipe"
)

const calQuestions = `{
  "u": {"type": "noul", "instructions": "?"},
  "d": {"type": "choice", "instructions": "?", "criteria": {"zed": null, "alpha": null, "mid": null}},
  "f": {"type": "score", "instructions": "?", "criteria": ["a", "b", "c"]}
}`

func TestCalibrateKeepsAnswersAndOrder(t *testing.T) {
	body := `{"model":"m","answers":{
	  "u":{"type":"noul","noul":0.99},
	  "d":{"type":"choice","choice":"alpha","probabilities":{"zed":0.01,"alpha":0.98,"mid":0.01},"confidence":0.95},
	  "f":{"type":"score","score":1.9,"probabilities":{"0":0.0,"1":0.1,"2":0.9},"legend":{"0":"a","1":"b","2":"c"},"confidence":0.8}
	}}`
	out, ok := calibrate([]byte(body), json.RawMessage(calQuestions), 2)
	if !ok {
		t.Fatal("expected a change")
	}
	var resp struct {
		Answers map[string]map[string]json.RawMessage
	}
	json.Unmarshal(out, &resp)

	var noul float64
	json.Unmarshal(resp.Answers["u"]["noul"], &noul)
	if want := 0.908; math.Abs(noul-want) > 0.001 { // sqrt(.99)/(sqrt(.99)+sqrt(.01))
		t.Errorf("noul %v, want %v", noul, want)
	}
	d := resp.Answers["d"]
	if string(d["choice"]) != `"alpha"` {
		t.Error("the chosen option must not change")
	}
	keys, _ := objectKeys(d["probabilities"])
	if len(keys) != 3 || keys[0] != "zed" || keys[1] != "alpha" {
		t.Errorf("option order must be kept, got %v", keys)
	}
	var raw struct{ Probabilities map[string]float64 }
	json.Unmarshal(d["x_raw"], &raw)
	if raw.Probabilities["alpha"] != 0.98 {
		t.Errorf("x_raw must keep the backend's numbers: %s", d["x_raw"])
	}
	var score float64
	json.Unmarshal(resp.Answers["f"]["score"], &score)
	if score >= 1.9 || score <= 1.0 {
		t.Errorf("score should be recomputed from the flatter distribution, got %v", score)
	}
	if string(resp.Answers["f"]["legend"]) != `{"0":"a","1":"b","2":"c"}` {
		t.Error("legend must pass through")
	}
}

func TestCalibrationPrecedence(t *testing.T) {
	s := &Server{}
	withRecipe := recipe.Ref{Name: "m", Recipe: &recipe.Recipe{Calibration: &recipe.Calibration{Temperature: 3}}}
	plain := recipe.Ref{Name: "p", Recipe: &recipe.Recipe{}}
	if t0, _ := s.calibrationFor(plain); t0 != 0 {
		t.Error("calibration must be off unless something turns it on")
	}
	if t0, src := s.calibrationFor(withRecipe); t0 != 3 || src != "recipe" {
		t.Errorf("recipe value: %v %s", t0, src)
	}
	s.net.settings.Calibration = map[string]float64{"m": 0}
	if t0, _ := s.calibrationFor(withRecipe); t0 != 0 {
		t.Error("the user's off must win over the recipe")
	}
	s.net.settings.Calibration["m"] = 1.5
	if t0, src := s.calibrationFor(withRecipe); t0 != 1.5 || src != "your setting" {
		t.Errorf("user value: %v %s", t0, src)
	}
}

// A model whose reported probabilities are its true ones sharpened by T=2 should fit back to about 2.
func TestFitRecoversTemperature(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var preds []bench.Prediction
	for i := 0; i < 3000; i++ {
		truth := []float64{rng.Float64(), rng.Float64(), rng.Float64()}
		var sum float64
		for _, v := range truth {
			sum += v * v * v
		}
		for j := range truth {
			truth[j] = truth[j] * truth[j] * truth[j] / sum
		}
		// Draw the label from the true distribution.
		r, label, acc := rng.Float64(), len(truth)-1, 0.0
		for j, v := range truth {
			if acc += v; r < acc {
				label = j
				break
			}
		}
		preds = append(preds, bench.Prediction{Variant: "clean", Label: label, P: bench.Temper(truth, 0.5)})
	}
	fit, ok := bench.FitTemperature(preds)
	if !ok || math.Abs(fit.Temperature-2) > 0.2 || fit.ECEAfter >= fit.ECEBefore {
		t.Errorf("fit %+v", fit)
	}
}
