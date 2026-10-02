package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"cyprid/internal/bench"
	"cyprid/internal/config"
	"cyprid/internal/recipe"
)

// Calibration is opt-in temperature scaling, per model. The user's setting (saved
// in config.json) wins over a recipe's; with neither, answers pass through
// untouched. A request can send "x_calibrate": false to get the raw answers.
//
// Temperature scaling raises each probability to 1/T and renormalises. It never
// changes which option is most likely, so accuracy is unchanged; it only makes
// confidence less (T > 1) or more (T < 1) extreme.

// calibrationFor returns the temperature to apply to ref (0 = none) and where it came from.
func (s *Server) calibrationFor(ref recipe.Ref) (float64, string) {
	s.net.mu.Lock()
	t, set := s.net.settings.Calibration[ref.Name]
	s.net.mu.Unlock()
	if set {
		if t <= 0 {
			return 0, "off (your setting)"
		}
		return t, "your setting"
	}
	if c := ref.Calibration(); c != nil && c.Temperature > 0 {
		return c.Temperature, "recipe"
	}
	return 0, "off"
}

func (s *Server) setCalibration(name string, t *float64) error {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	if s.net.settings.Calibration == nil {
		s.net.settings.Calibration = map[string]float64{}
	}
	if t == nil {
		delete(s.net.settings.Calibration, name) // back to the recipe's value, if any
	} else {
		s.net.settings.Calibration[name] = *t
	}
	return config.Save(s.net.settings)
}

// calibrate applies temperature t to every answer in a Jev response body.
func calibrate(body []byte, questions json.RawMessage, t float64) ([]byte, bool) {
	var obj map[string]json.RawMessage
	var answers map[string]json.RawMessage
	var qs map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || json.Unmarshal(obj["answers"], &answers) != nil || json.Unmarshal(questions, &qs) != nil {
		return body, false
	}
	changed := false
	for id, raw := range answers {
		if fixed, ok := calibrateAnswer(raw, qs[id], t); ok {
			answers[id] = fixed
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	obj["answers"], _ = json.Marshal(answers)
	out, _ := json.Marshal(obj)
	return out, true
}

func entropyConfidence(p []float64) float64 {
	if len(p) < 2 {
		return 1
	}
	var h float64
	for _, v := range p {
		if v > 0 {
			h -= v * math.Log(v)
		}
	}
	return math.Max(0, 1-h/math.Log(float64(len(p))))
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func calibrateAnswer(raw, q json.RawMessage, t float64) (json.RawMessage, bool) {
	var a map[string]json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return nil, false
	}
	var typ string
	json.Unmarshal(a["type"], &typ)

	// Keep what the backend said, so the change is auditable and reversible.
	rawCopy := map[string]json.RawMessage{}
	for _, k := range []string{"noul", "probabilities", "confidence", "score"} {
		if v, ok := a[k]; ok {
			rawCopy[k] = v
		}
	}

	switch typ {
	case "noul":
		var p float64
		if json.Unmarshal(a["noul"], &p) != nil {
			return nil, false
		}
		c := bench.Temper([]float64{1 - p, p}, t)
		a["noul"], _ = json.Marshal(round6(c[1]))
		if _, ok := a["confidence"]; ok {
			a["confidence"], _ = json.Marshal(round6(math.Max(c[0], c[1])))
		}
	case "choice", "score":
		var probs map[string]float64
		if json.Unmarshal(a["probabilities"], &probs) != nil || len(probs) == 0 {
			return nil, false
		}
		// Keep the backend's option order (Go maps would sort the keys).
		keys, _ := objectKeys(a["probabilities"])
		if typ == "score" {
			sort.Slice(keys, func(i, j int) bool { ai, _ := strconv.Atoi(keys[i]); aj, _ := strconv.Atoi(keys[j]); return ai < aj })
		}
		p := make([]float64, len(keys))
		for i, k := range keys {
			p[i] = probs[k]
		}
		c := bench.Temper(p, t)
		a["probabilities"] = orderedObject(keys, func(i int) any { return round6(c[i]) })
		a["confidence"], _ = json.Marshal(round6(entropyConfidence(c)))
		if typ == "score" {
			var score float64
			for i, k := range keys {
				level, _ := strconv.Atoi(k)
				score += float64(level) * c[i]
			}
			a["score"], _ = json.Marshal(round6(score))
		}
	default:
		return nil, false
	}
	a["x_calibrated"], _ = json.Marshal(map[string]float64{"temperature": t})
	a["x_raw"], _ = json.Marshal(rawCopy)
	out, _ := json.Marshal(a)
	return out, true
}

// ---- admin API ----

type calibrationSuggestion struct {
	Run       string    `json:"run"`
	Suite     string    `json:"suite"`
	StartedAt time.Time `json:"started_at"`
	bench.Fit
}

// adminCalibration reports a model's calibration and temperatures fitted from saved benchmark runs.
func (s *Server) adminCalibration(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.recipes().Resolve(r.PathValue("name"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", r.PathValue("name")))
		return
	}
	t, source := s.calibrationFor(ref)
	var recipeT float64
	if c := ref.Calibration(); c != nil {
		recipeT = c.Temperature
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model": ref.Name, "temperature": t, "source": source, "recipe_temperature": recipeT,
		"suggestions": suggestions(ref.Name),
	})
}

func suggestions(model string) []calibrationSuggestion {
	var out []calibrationSuggestion
	dirs, _ := filepath.Glob(filepath.Join(benchDir(), "*"))
	for _, d := range dirs {
		preds := readPredictionsFile(filepath.Join(d, "predictions-"+model+".jsonl"))
		fit, ok := bench.FitTemperature(preds)
		if !ok {
			continue
		}
		var rep struct {
			Suite     string    `json:"suite"`
			StartedAt time.Time `json:"started_at"`
		}
		if data, err := os.ReadFile(filepath.Join(d, "report.json")); err == nil {
			json.Unmarshal(data, &rep)
		}
		out = append(out, calibrationSuggestion{Run: filepath.Base(d), Suite: rep.Suite, StartedAt: rep.StartedAt, Fit: fit})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

func readPredictionsFile(path string) []bench.Prediction {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []bench.Prediction
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var p bench.Prediction
		if json.Unmarshal(sc.Bytes(), &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// adminSetCalibration: {"temperature": 2.2} turns it on, {"temperature": 0} turns it
// off (even if the recipe sets one), {"temperature": null} returns to the recipe's value.
func (s *Server) adminSetCalibration(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.recipes().Resolve(r.PathValue("name"))
	if !ok {
		fail(w, http.StatusNotFound, fmt.Sprintf("unknown model %q", r.PathValue("name")))
		return
	}
	var body struct {
		Temperature *float64 `json:"temperature"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, `body must be {"temperature": number | 0 | null}`)
		return
	}
	if t := body.Temperature; t != nil && (*t < 0 || *t > 20 || math.IsNaN(*t)) {
		fail(w, http.StatusUnprocessableEntity, "temperature must be between 0 (off) and 20")
		return
	}
	if err := s.setCalibration(ref.Name, body.Temperature); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	t, source := s.calibrationFor(ref)
	writeJSON(w, http.StatusOK, map[string]any{"model": ref.Name, "temperature": t, "source": source})
}
