package bench

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type ModelReport struct {
	Model      string             `json:"model"`
	Clean      Metrics            `json:"clean"`
	ByType     map[string]Metrics `json:"by_type"`
	ByTask     map[string]Metrics `json:"by_task"`
	ByTier     map[string]Metrics `json:"by_tier,omitempty"`
	Robustness Robustness         `json:"robustness"`
	Latency    Latency            `json:"latency_ms"`
	Emulated   []string           `json:"emulated_types,omitempty"`
	Truncated  int                `json:"truncated_answers,omitempty"`      // answers missing some options' probabilities
	Calibrated float64            `json:"calibrated_temperature,omitempty"` // temperature the gateway applied, if any
	FirstError string             `json:"first_error,omitempty"`
}

type Report struct {
	Suite     string               `json:"suite"`
	Records   int                  `json:"records"`
	Partial   bool                 `json:"partial,omitempty"` // the run was interrupted; some models answered only some records
	StartedAt time.Time            `json:"started_at"`
	Models    []ModelReport        `json:"models"`
	Published map[string]Published `json:"published,omitempty"`
}

func Summarise(model string, preds []Prediction) ModelReport {
	r := ModelReport{Model: model, ByType: map[string]Metrics{}, ByTask: map[string]Metrics{}, ByTier: map[string]Metrics{}}
	var clean []Prediction
	byType, byTask, byTier := map[string][]Prediction{}, map[string][]Prediction{}, map[string][]Prediction{}
	emulated := map[string]bool{}
	for _, p := range preds {
		if p.Error != "" && r.FirstError == "" {
			r.FirstError = p.Error
		}
		if p.Emulated {
			emulated[p.Type] = true
		}
		if p.Truncated {
			r.Truncated++
		}
		if p.Calibrated > 0 {
			r.Calibrated = p.Calibrated
		}
		if p.Variant != "clean" {
			continue
		}
		clean = append(clean, p)
		byType[p.Type] = append(byType[p.Type], p)
		byTask[p.Task] = append(byTask[p.Task], p)
		if p.Tier != "" {
			byTier[p.Tier] = append(byTier[p.Tier], p)
		}
	}
	r.Clean = Compute(clean)
	for k, v := range byType {
		r.ByType[k] = Compute(v)
	}
	for k, v := range byTask {
		r.ByTask[k] = Compute(v)
	}
	for k, v := range byTier {
		r.ByTier[k] = Compute(v)
	}
	r.Robustness = ComputeRobustness(preds)
	r.Latency = ComputeLatency(preds)
	for t := range emulated {
		r.Emulated = append(r.Emulated, t)
	}
	sort.Strings(r.Emulated)
	return r
}

// Print writes the report as tables: models side by side, published results alongside.
func (rep Report) Print(w io.Writer) {
	cols := []string{}
	for _, m := range rep.Models {
		cols = append(cols, m.Model)
	}
	var pubNames []string
	for name := range rep.Published {
		pubNames = append(pubNames, name)
	}
	sort.Strings(pubNames)
	showPub := true // published columns only in tables that have published numbers
	header := func(tw io.Writer, first string) {
		fmt.Fprint(tw, first)
		for _, c := range cols {
			fmt.Fprint(tw, "\t"+c)
		}
		for _, p := range pubNames {
			if showPub {
				fmt.Fprint(tw, "\t"+p+" (published)")
			}
		}
		fmt.Fprint(tw, "\t\n")
	}
	pct := func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }
	opt := func(v *float64, f func(float64) string) string {
		if v == nil {
			return "-"
		}
		return f(*v)
	}
	num := func(v float64) string { return fmt.Sprintf("%.3f", v) }

	fmt.Fprintf(w, "\nSuite %s: %d records. Headline numbers use the clean records only.\n", rep.Suite, rep.Records)
	if rep.Partial {
		fmt.Fprintf(w, "PARTIAL: the run was interrupted, so some models answered only some records (see \"questions scored\").\n")
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	header(tw, "")
	row := func(label string, f func(ModelReport) string, pub func(Published) string) {
		fmt.Fprint(tw, label)
		for _, m := range rep.Models {
			fmt.Fprint(tw, "\t"+f(m))
		}
		for _, p := range pubNames {
			if !showPub {
				continue
			}
			s := "-"
			if pub != nil {
				s = pub(rep.Published[p])
			}
			fmt.Fprint(tw, "\t"+s)
		}
		fmt.Fprint(tw, "\t\n")
	}
	row("questions scored", func(m ModelReport) string { return fmt.Sprint(m.Clean.N) }, nil)
	row("failed requests", func(m ModelReport) string { return fmt.Sprint(m.Clean.Errors) }, nil)
	row("accuracy", func(m ModelReport) string { return pct(m.Clean.Acc) }, func(p Published) string { return pct(p.Acc) })
	row("brier (lower better)", func(m ModelReport) string { return num(m.Clean.Brier) }, func(p Published) string {
		if p.Brier == 0 {
			return "-"
		}
		return num(p.Brier)
	})
	row("ECE (lower better)", func(m ModelReport) string { return num(m.Clean.ECE) }, nil)
	row("mean confidence", func(m ModelReport) string { return pct(m.Clean.MeanConf) }, nil)
	row("answered at >=0.9", func(m ModelReport) string { return pct(m.Clean.CoverageAt09) }, nil)
	row("  accuracy of those", func(m ModelReport) string { return opt(m.Clean.AccAt09, pct) }, nil)
	row("confident errors", func(m ModelReport) string { return pct(m.Clean.ConfidentErrorRate) }, nil)
	row("latency p50 (ms)", func(m ModelReport) string { return fmt.Sprintf("%.0f", m.Latency.P50) }, nil)
	row("latency p95 (ms)", func(m ModelReport) string { return fmt.Sprintf("%.0f", m.Latency.P95) }, nil)
	tw.Flush()

	tiers := map[string]bool{}
	for _, m := range rep.Models {
		for t := range m.ByTier {
			tiers[t] = true
		}
	}
	showPub = false
	if len(tiers) > 0 {
		fmt.Fprintf(w, "\nAccuracy by tier\n\n")
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
		header(tw, "")
		for _, t := range []string{"easy", "standard", "judge", "hard"} {
			if !tiers[t] {
				continue
			}
			row(t, func(m ModelReport) string {
				mt, ok := m.ByTier[t]
				if !ok {
					return "-"
				}
				return fmt.Sprintf("%s (n=%d)", pct(mt.Acc), mt.N)
			}, nil)
		}
		tw.Flush()
	}

	showPub = false
	fmt.Fprintf(w, "\nAccuracy by question type\n\n")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	header(tw, "")
	for _, t := range []string{"noul", "choice", "score"} {
		row(t, func(m ModelReport) string {
			mt, ok := m.ByType[t]
			if !ok {
				return "-"
			}
			s := pct(mt.Acc)
			for _, e := range m.Emulated {
				if e == t {
					s += " (emulated)"
				}
			}
			return s
		}, nil)
	}
	row("score MAE (levels)", func(m ModelReport) string { return opt(m.ByType["score"].ScoreMAE, num) }, nil)
	tw.Flush()

	showPub = false
	for _, p := range rep.Published {
		showPub = showPub || len(p.Tasks) > 0
	}
	fmt.Fprintf(w, "\nAccuracy by task\n\n")
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
	header(tw, "")
	tasks := map[string]bool{}
	for _, m := range rep.Models {
		for t := range m.ByTask {
			tasks[t] = true
		}
	}
	var taskNames []string
	for t := range tasks {
		taskNames = append(taskNames, t)
	}
	sort.Strings(taskNames)
	for _, t := range taskNames {
		row(t, func(m ModelReport) string {
			mt, ok := m.ByTask[t]
			if !ok {
				return "-"
			}
			return fmt.Sprintf("%s (n=%d)", pct(mt.Acc), mt.N)
		}, func(p Published) string {
			if v, ok := p.Tasks[t]; ok {
				return pct(v)
			}
			return "-"
		})
	}
	tw.Flush()

	showPub = false
	hasVariants := false
	for _, m := range rep.Models {
		r := m.Robustness
		hasVariants = hasVariants || r.PermutedN+r.NonePresentN+r.NoneAbsentN > 0
	}
	if hasVariants {
		fmt.Fprintf(w, "\nRobustness (variant records vs their clean originals)\n\n")
		tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.AlignRight)
		header(tw, "")
		row("answer flips when options reordered", func(m ModelReport) string { return opt(m.Robustness.PermutedFlip, pct) }, nil)
		row("right with 'none of these' added", func(m ModelReport) string { return opt(m.Robustness.NonePresentAcc, pct) }, nil)
		row("picks 'none' when answer removed", func(m ModelReport) string { return opt(m.Robustness.NoneAbsentAcc, pct) }, nil)
		tw.Flush()
	}

	for _, m := range rep.Models {
		if m.Calibrated > 0 {
			fmt.Fprintf(w, "\n%s: answers were calibrated by cyprid (temperature %.2f); raw probabilities are kept in the predictions.\n", m.Model, m.Calibrated)
		}
		if m.Truncated > 0 {
			fmt.Fprintf(w, "\n%s: %d answers listed only some options' probabilities; they were renormalised over the listed options.\n", m.Model, m.Truncated)
		}
		if m.FirstError != "" {
			fmt.Fprintf(w, "\n%s: %d failed; first error: %s\n", m.Model, m.Clean.Errors, truncate(m.FirstError, 300))
		}
	}
	for _, p := range pubNames {
		pub := rep.Published[p]
		fmt.Fprintf(w, "\n%s (published): %s, on the same items.", p, pub.Source)
		if pub.Note != "" {
			fmt.Fprintf(w, " Note: %s.", pub.Note)
		}
	}
	fmt.Fprintln(w)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
