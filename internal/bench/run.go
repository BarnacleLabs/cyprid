package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Prediction is one answered (or failed) question.
type Prediction struct {
	ID        string    `json:"id"`
	Parent    string    `json:"parent,omitempty"`
	Variant   string    `json:"variant"`
	Tier      string    `json:"tier,omitempty"`
	Question  string    `json:"question"`
	Task      string    `json:"task"`
	Type      string    `json:"type"`
	Keys      []string  `json:"keys"`
	Label     int       `json:"label"`
	P         []float64 `json:"p,omitempty"`
	Emulated  bool      `json:"emulated,omitempty"`
	Truncated bool      `json:"truncated,omitempty"` // backend reported only some options; P is renormalised over them
	// Calibrated is the temperature the gateway applied (0 = none); RawP is the distribution before it.
	Calibrated float64   `json:"calibrated,omitempty"`
	RawP       []float64 `json:"raw_p,omitempty"`
	LatencyMs  float64   `json:"latency_ms"`
	Error      string    `json:"error,omitempty"`
}

type Runner struct {
	ServerURL   string
	AdminToken  string // for the admin call that loads a model before timing
	Concurrency int
	Progress    func(done, total int)
	// OnRecord, if set, receives each record's predictions as soon as they are in,
	// so a run can be saved as it goes. Calls are serialised.
	OnRecord func([]Prediction)
	client   *http.Client
}

// Warm loads a local model before timing starts, so the first records don't pay for the cold start.
func (r *Runner) Warm(model string) error {
	req, _ := http.NewRequest("POST", r.ServerURL+"/admin/models/"+model+"/start?wait=1", nil)
	req.Header.Set("X-Cyprid-Token", r.AdminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("server not reachable at %s: %w", r.ServerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest { // remote model: nothing to warm
		return nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("starting %s: %s", model, bytes.TrimSpace(body))
	}
	return nil
}

func (r *Runner) Run(ctx context.Context, model string, recs []Record) []Prediction {
	if r.client == nil {
		r.client = &http.Client{Timeout: 10 * time.Minute}
	}
	conc := max(r.Concurrency, 1)
	out := make([][]Prediction, len(recs))
	var done atomic.Int64
	var saveMu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				preds := r.one(ctx, model, recs[i])
				if ctx.Err() != nil {
					continue // interrupted mid-request: not an answer, so don't record it
				}
				out[i] = preds
				if r.OnRecord != nil {
					saveMu.Lock()
					r.OnRecord(preds)
					saveMu.Unlock()
				}
				if r.Progress != nil {
					r.Progress(int(done.Add(1)), len(recs))
				}
			}
		}()
	}
	for i := range recs {
		if ctx.Err() != nil {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var flat []Prediction
	for _, ps := range out {
		flat = append(flat, ps...)
	}
	return flat
}

type answer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
	Emulated      bool               `json:"x_emulated"`
	Truncated     bool               `json:"x_truncated"`
	Calibrated    *struct {
		Temperature float64 `json:"temperature"`
	} `json:"x_calibrated"`
	Raw *answer `json:"x_raw"`
}

func (r *Runner) one(ctx context.Context, model string, rec Record) []Prediction {
	preds := make(map[string]*Prediction)
	for id, q := range rec.Questions {
		keys, label, _ := q.Keys() // validated at load
		preds[id] = &Prediction{ID: rec.Meta.ID, Parent: rec.Meta.ParentID, Variant: rec.Meta.Variant, Tier: rec.Meta.Tier,
			Question: id, Task: q.Task, Type: q.Type, Keys: keys, Label: label}
	}
	fail := func(msg string) []Prediction {
		var out []Prediction
		for _, p := range preds {
			p.Error = msg
			out = append(out, *p)
		}
		return out
	}

	body, err := rec.Request(model)
	if err != nil {
		return fail(err.Error())
	}
	var resp struct {
		Answers map[string]answer `json:"answers"`
		Detail  any               `json:"detail"`
	}
	start := time.Now()
	status, data, err := r.post(ctx, body)
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		return fail(err.Error())
	}
	if status != 200 {
		return fail(fmt.Sprintf("HTTP %d: %s", status, bytes.TrimSpace(data)))
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fail("bad response: " + err.Error())
	}

	var out []Prediction
	for id, p := range preds {
		p.LatencyMs = elapsed
		a, ok := resp.Answers[id]
		if !ok {
			p.Error = "no answer returned"
		} else if dist, err := distribution(p, a); err != nil {
			p.Error = err.Error()
		} else {
			p.P, p.Emulated, p.Truncated = dist, a.Emulated, a.Truncated
			if a.Calibrated != nil && a.Raw != nil {
				raw := *a.Raw
				raw.Type = a.Type
				if rawDist, err := distribution(p, raw); err == nil {
					p.Calibrated, p.RawP = a.Calibrated.Temperature, rawDist
				}
			}
		}
		out = append(out, *p)
	}
	return out
}

// post sends one request, retrying while the model loads (503) or the backend is rate limited (429/529).
func (r *Runner) post(ctx context.Context, body []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.ServerURL+"/v1/systemone", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := r.client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		retryable := resp.StatusCode == 503 || resp.StatusCode == 429 || resp.StatusCode == 529
		if !retryable || attempt >= 8 {
			return resp.StatusCode, data, err
		}
		wait := time.Duration(1<<min(attempt, 5)) * time.Second
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(s) * time.Second
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		}
	}
}

// distribution turns an answer into probabilities over the prediction's keys.
func distribution(p *Prediction, a answer) ([]float64, error) {
	if a.Type != p.Type {
		return nil, fmt.Errorf("answer type %q, want %q", a.Type, p.Type)
	}
	var dist []float64
	if p.Type == "noul" {
		if a.Noul == nil {
			return nil, fmt.Errorf("noul answer has no value")
		}
		dist = []float64{1 - *a.Noul, *a.Noul}
	} else {
		for _, k := range p.Keys {
			v, ok := a.Probabilities[k]
			if !ok {
				return nil, fmt.Errorf("no probability for option %q", k)
			}
			dist = append(dist, v)
		}
	}
	total := 0.0
	for _, v := range dist {
		if v < -1e-9 || v > 1+1e-9 || math.IsNaN(v) {
			return nil, fmt.Errorf("probability %v out of range", v)
		}
		total += v
	}
	// A truncated answer (the backend listed only its top options) is missing mass by design.
	if math.Abs(total-1) > 0.01 && !(a.Truncated && total > 0 && total <= 1.01) {
		return nil, fmt.Errorf("probabilities sum to %.4f", total)
	}
	for i := range dist {
		dist[i] = math.Max(dist[i], 0) / total
	}
	return dist, nil
}
