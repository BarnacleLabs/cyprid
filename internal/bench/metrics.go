package bench

import (
	"math"
	"sort"
)

// Metrics follow Kev's kev/metrics.py so numbers are comparable with its
// published results: confidence is the top probability, ECE uses 10 equal-width
// bins, Brier is summed over options, and a confident error is a wrong answer
// with confidence >= 0.9.
type Metrics struct {
	N                  int      `json:"n"`
	Errors             int      `json:"errors"` // requests that failed; excluded from the other numbers
	Acc                float64  `json:"acc"`
	Brier              float64  `json:"brier"`
	ECE                float64  `json:"ece"`
	MeanConf           float64  `json:"mean_conf"`
	ConfidentErrorRate float64  `json:"confident_error_rate"`
	CoverageAt09       float64  `json:"coverage_at_0_9"`
	AccAt09            *float64 `json:"acc_at_0_9,omitempty"`
	ScoreMAE           *float64 `json:"score_mae,omitempty"`
}

func argmax(p []float64) int {
	best := 0
	for i, v := range p {
		if v > p[best] {
			best = i
		}
	}
	return best
}

func Compute(preds []Prediction) Metrics {
	var m Metrics
	var conf []float64
	var correct []bool
	var brier, maeSum float64
	var maeN, high, highCorrect, confidentErr int
	for _, p := range preds {
		if p.Error != "" || p.P == nil {
			m.Errors++
			continue
		}
		c := p.P[argmax(p.P)]
		ok := argmax(p.P) == p.Label
		conf, correct = append(conf, c), append(correct, ok)
		for i, v := range p.P {
			t := 0.0
			if i == p.Label {
				t = 1
			}
			brier += (v - t) * (v - t)
		}
		if c >= 0.9 {
			high++
			if ok {
				highCorrect++
			} else {
				confidentErr++
			}
		}
		if p.Type == "score" {
			exp := 0.0
			for i, v := range p.P {
				exp += float64(i) * v
			}
			maeSum += math.Abs(exp - float64(p.Label))
			maeN++
		}
	}
	m.N = len(conf)
	if m.N == 0 {
		return m
	}
	n := float64(m.N)
	nCorrect := 0
	for i := range conf {
		m.MeanConf += conf[i] / n
		if correct[i] {
			nCorrect++
		}
	}
	m.Acc = float64(nCorrect) / n
	m.Brier = brier / n
	m.ECE = ece(conf, correct)
	m.ConfidentErrorRate = float64(confidentErr) / n
	m.CoverageAt09 = float64(high) / n
	if high > 0 {
		v := float64(highCorrect) / float64(high)
		m.AccAt09 = &v
	}
	if maeN > 0 {
		v := maeSum / float64(maeN)
		m.ScoreMAE = &v
	}
	return m
}

func ece(conf []float64, correct []bool) float64 {
	const bins = 10
	var e float64
	for b := 0; b < bins; b++ {
		lo, hi := float64(b)/bins, float64(b+1)/bins
		var n, sumConf, sumOK float64
		for i, c := range conf {
			in := c >= lo && c < hi
			if b == bins-1 {
				in = c >= lo && c <= hi
			}
			if in {
				n++
				sumConf += c
				if correct[i] {
					sumOK++
				}
			}
		}
		if n > 0 {
			e += n / float64(len(conf)) * math.Abs(sumOK/n-sumConf/n)
		}
	}
	return e
}

// Robustness compares variant records with their clean parents.
type Robustness struct {
	PermutedN      int      `json:"permuted_n"`
	PermutedFlip   *float64 `json:"permuted_flip_rate,omitempty"` // answer changed when only the option order changed
	NonePresentN   int      `json:"none_present_n"`
	NonePresentAcc *float64 `json:"none_present_acc,omitempty"` // right answer still chosen with a "none of these" option added
	NoneAbsentN    int      `json:"none_absent_n"`
	NoneAbsentAcc  *float64 `json:"none_absent_acc,omitempty"` // "none of these" chosen when the right answer is removed
}

func ComputeRobustness(preds []Prediction) Robustness {
	var r Robustness
	clean := map[string]Prediction{}
	for _, p := range preds {
		if p.Variant == "clean" && p.Error == "" {
			clean[p.ID+"/"+p.Question] = p
		}
	}
	var flips, perm, npOK, np, naOK, na int
	for _, p := range preds {
		if p.Error != "" || p.P == nil || p.Variant == "clean" {
			continue
		}
		ok := argmax(p.P) == p.Label
		switch p.Variant {
		case "permuted":
			parent, found := clean[p.Parent+"/"+p.Question]
			if !found {
				continue
			}
			perm++
			if p.Keys[argmax(p.P)] != parent.Keys[argmax(parent.P)] {
				flips++
			}
		case "none_present":
			np++
			if ok {
				npOK++
			}
		case "none_absent":
			na++
			if ok {
				naOK++
			}
		}
	}
	rate := func(a, b int) *float64 {
		if b == 0 {
			return nil
		}
		v := float64(a) / float64(b)
		return &v
	}
	r.PermutedN, r.PermutedFlip = perm, rate(flips, perm)
	r.NonePresentN, r.NonePresentAcc = np, rate(npOK, np)
	r.NoneAbsentN, r.NoneAbsentAcc = na, rate(naOK, na)
	return r
}

type Latency struct {
	P50, P95, Mean float64
}

// ComputeLatency is per request; a record's questions share one request.
func ComputeLatency(preds []Prediction) Latency {
	seen := map[string]bool{}
	var ms []float64
	for _, p := range preds {
		if p.Error != "" || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		ms = append(ms, p.LatencyMs)
	}
	if len(ms) == 0 {
		return Latency{}
	}
	sort.Float64s(ms)
	var sum float64
	for _, v := range ms {
		sum += v
	}
	q := func(f float64) float64 { return ms[min(len(ms)-1, int(f*float64(len(ms))))] }
	return Latency{P50: q(0.5), P95: q(0.95), Mean: sum / float64(len(ms))}
}
