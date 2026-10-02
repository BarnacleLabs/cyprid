package bench

import (
	"math"
)

// Temper applies temperature scaling to a distribution: p_i^(1/T), renormalised.
// It never changes which option is most likely, only how extreme the distribution is.
func Temper(p []float64, t float64) []float64 {
	if t == 1 || t <= 0 {
		return append([]float64(nil), p...)
	}
	out := make([]float64, len(p))
	maxZ := math.Inf(-1)
	for i, v := range p {
		out[i] = math.Log(math.Max(v, 1e-12)) / t
		maxZ = math.Max(maxZ, out[i])
	}
	var sum float64
	for i := range out {
		out[i] = math.Exp(out[i] - maxZ)
		sum += out[i]
	}
	for i := range out {
		out[i] /= sum
	}
	return out
}

// MinFitItems is the fewest answered items a temperature is fitted from: below
// that, a run of easy, all-correct answers fits a meaningless extreme value.
const MinFitItems = 100

// Fit is the temperature that best calibrates a model on a set of predictions.
type Fit struct {
	Temperature float64 `json:"temperature"`
	N           int     `json:"n"`
	ECEBefore   float64 `json:"ece_before"`
	ECEAfter    float64 `json:"ece_after"`
	// Confident errors (wrong at >= 0.9) as a share of all predictions, before and after.
	ConfidentErrorsBefore float64 `json:"confident_errors_before"`
	ConfidentErrorsAfter  float64 `json:"confident_errors_after"`
}

// FitTemperature finds the temperature minimising negative log-likelihood on the
// clean, answered predictions, using each prediction's raw (uncalibrated)
// probabilities, so a run made with calibration on can still be refitted.
func FitTemperature(preds []Prediction) (Fit, bool) {
	var rows []Prediction
	for _, p := range preds {
		if p.Error == "" && p.P != nil && p.Variant == "clean" {
			if p.RawP != nil {
				p.P = p.RawP
			}
			rows = append(rows, p)
		}
	}
	if len(rows) < MinFitItems {
		return Fit{}, false // too few to fit anything meaningful
	}
	nll := func(t float64) float64 {
		var s float64
		for _, r := range rows {
			s -= math.Log(math.Max(Temper(r.P, t)[r.Label], 1e-12))
		}
		return s
	}
	best, bestNLL := 1.0, nll(1)
	for t := 0.25; t <= 10.0001; t += 0.05 {
		if v := nll(t); v < bestNLL {
			best, bestNLL = t, v
		}
	}
	best = math.Round(best*100) / 100
	metrics := func(t float64) Metrics {
		tempered := make([]Prediction, len(rows))
		for i, r := range rows {
			r.P = Temper(r.P, t)
			tempered[i] = r
		}
		return Compute(tempered)
	}
	before, after := metrics(1), metrics(best)
	return Fit{
		Temperature: best, N: len(rows),
		ECEBefore: before.ECE, ECEAfter: after.ECE,
		ConfidentErrorsBefore: before.ConfidentErrorRate, ConfidentErrorsAfter: after.ConfidentErrorRate,
	}, true
}
