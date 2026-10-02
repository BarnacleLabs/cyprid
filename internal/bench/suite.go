// Package bench scores models served by cyprid against labelled suites.
//
// A suite is JSONL in Kev's format: each record is a Jev request whose questions
// also carry "label" (the right answer) and "src" (the task), plus "_meta" with
// "id" and "variant". Records whose variant is "clean" make up the headline
// numbers; the others (options permuted, a "none of these" option added or
// removed) are robustness probes, reported separately.
package bench

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"cyprid/internal/paths"
)

// BuiltinSuite is a public suite fetched on first use and verified by hash.
type BuiltinSuite struct {
	Name        string
	Description string
	Format      string // "kev" (the default) or "jevbench"
	Files       []SuiteFile
	// Published results for models we may not be able to run (e.g. Jev without an API key).
	Published map[string]Published
}

type SuiteFile struct {
	Tier   string // reported as its own breakdown; empty for single-file suites
	URL    string
	SHA256 string
}

type Published struct {
	Acc    float64
	Brier  float64 // 0 when not published
	Tasks  map[string]float64
	Source string
	Note   string // e.g. when the published run used a different version or hardware
}

// kevRevision pins the Kev commit the suite (and its published numbers) come from.
const kevRevision = "5920c5fe4ca8e0970ed4209ac2c9b8e18bea5109"

var Builtins = map[string]BuiltinSuite{
	"kev-transfer-v4": {
		Name: "kev-transfer-v4",
		Description: "Kev's 'new sources' suite: 764 questions from datasets and rule types Kev never trained on " +
			"(MMLU, SciQ, Emotion, TweetEval, QNLI, PAWS, held-out policies and rule compositions). Development split.",
		Files: []SuiteFile{{
			URL:    "https://raw.githubusercontent.com/jaredpalmer/kev/" + kevRevision + "/evals/v4/transfer-v4/development.jsonl",
			SHA256: "ff374c49c6c9f15f8a56fb274b4a4857d20497eb8dd1ac07ce01560e682a5f2e",
		}},
		Published: map[string]Published{
			// From docs/kev-family-summary.json at kevRevision. Jev was run by Kev's author against TypeSafe's API.
			"jev": {Acc: 0.8567, Brier: 0.2110, Source: "Kev repo, runs/jev-transfer-v4", Tasks: map[string]float64{
				"composition_held_and_or": 0.906, "composition_held_conditional": 0.781, "composition_held_or_not": 0.969,
				"contrastive_authorization": 1.0, "contrastive_deadline": 0.925, "emotion": 0.588, "mmlu": 0.9,
				"paws": 0.788, "qnli": 0.925, "sciq": 0.988, "tweet_offensive": 0.813}},
			"kev-0.8b": {Acc: 0.6479, Brier: 0.4813, Source: "Kev repo, docs/kev-family-summary.json", Tasks: map[string]float64{
				"composition_held_and_or": 0.656, "composition_held_conditional": 0.656, "composition_held_or_not": 0.563,
				"contrastive_authorization": 0.95, "contrastive_deadline": 0.35, "emotion": 0.5, "mmlu": 0.425,
				"paws": 0.6, "qnli": 0.813, "sciq": 0.9, "tweet_offensive": 0.675}},
			"kev-4b": {Acc: 0.8171, Brier: 0.2691, Source: "Kev repo, docs/kev-family-summary.json", Tasks: map[string]float64{
				"composition_held_and_or": 1.0, "composition_held_conditional": 1.0, "composition_held_or_not": 0.938,
				"contrastive_authorization": 1.0, "contrastive_deadline": 0.65, "emotion": 0.55, "mmlu": 0.725,
				"paws": 0.788, "qnli": 0.913, "sciq": 0.975, "tweet_offensive": 0.75}},
		},
	},
	"jevbench-public": {
		Name: "jevbench-public",
		Description: "JevBench's public items (Benchmark Heaven, MIT): 231 decisions in three tiers, easy (48), " +
			"standard (72) and hard (111: long policies, multi-hop, temporal and numeric reasoning, traps). Not the " +
			"official JevBench score, which also uses sealed and licensed items and speed and cost axes.",
		Format: "jevbench",
		Files: []SuiteFile{
			{Tier: "easy", URL: jevbenchRaw + "easy.jsonl", SHA256: "231df3c2c8e88a1a8c137ebe85de96ba70fabd330849098ac7b3c52c70b7172b"},
			{Tier: "standard", URL: jevbenchRaw + "original.jsonl", SHA256: "5c2414edb3006b8bfcb70fda433f0f9ca015759433849f8d3104328a1f7c4180"},
			{Tier: "hard", URL: jevbenchRaw + "hard.jsonl", SHA256: "89e9e6becb33ed88c1de7d42dcc87531b2fb64cfaef4e1986faf7c37b3f80ebb"},
		},
		// public_accuracy from results/v1.4.2.1/jevbench-v1.4.2.1-results.json at jevbenchRevision.
		Published: map[string]Published{
			"jev":        {Acc: 0.8658, Source: "JevBench v1.4.2.1, Jev 1.13.0"},
			"decider-4b": {Acc: 0.8355, Source: "JevBench v1.4.2.1, decider-4b v2", Note: "published run may be an earlier revision than the weights installed here"},
			"decider-2b": {Acc: 0.7100, Source: "JevBench v1.4.2.1, decider-2b"},
			"openjev":    {Acc: 0.8182, Source: "JevBench v1.4.2.1, OpenJev", Note: "published run used the NVFP4 weights on vLLM (NVIDIA); here it is the 4-bit MLX build"},
			"laya":       {Acc: 0.5844, Source: "JevBench v1.4.2.1, Laya ModernBERT-large 421M"},
			"von":        {Acc: 0.5714, Source: "JevBench v1.4.2.1, Von", Note: "published run may predate Von 1.2"},
		},
	},
}

const jevbenchRevision = "d06ee95988da1350eb8ae5511daa0e06bfffe911"
const jevbenchRaw = "https://raw.githubusercontent.com/fstandhartinger/jevbench/" + jevbenchRevision + "/datasets/public/"

type Record struct {
	State     json.RawMessage          `json:"state"`
	Questions map[string]LabelledQuery `json:"questions"`
	Meta      struct {
		ID       string `json:"id"`
		Variant  string `json:"variant"`
		ParentID string `json:"parent_id"`
		Tier     string `json:"tier,omitempty"`
	} `json:"_meta"`
}

type LabelledQuery struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
	Label        json.RawMessage `json:"label"`
	Task         string          `json:"src"`
}

// Request returns the record as a plain Jev request, without labels.
func (r Record) Request(model string) ([]byte, error) {
	qs := map[string]any{}
	for id, q := range r.Questions {
		m := map[string]any{"type": q.Type, "instructions": q.Instructions}
		if len(q.Criteria) > 0 {
			m["criteria"] = q.Criteria
		}
		qs[id] = m
	}
	return json.Marshal(map[string]any{"model": model, "state": r.State, "questions": qs})
}

// Keys is the ordered outcome space of a question and the index of its label:
// noul is [false, true], choice the criteria keys in order, score the level indices.
func (q LabelledQuery) Keys() (keys []string, label int, err error) {
	switch q.Type {
	case "noul":
		var y bool
		if err := json.Unmarshal(q.Label, &y); err != nil {
			return nil, 0, fmt.Errorf("noul label: %w", err)
		}
		if y {
			label = 1
		}
		return []string{"false", "true"}, label, nil
	case "choice":
		keys, err = orderedKeys(q.Criteria)
		if err != nil {
			return nil, 0, err
		}
		var y string
		if err := json.Unmarshal(q.Label, &y); err != nil {
			return nil, 0, fmt.Errorf("choice label: %w", err)
		}
		for i, k := range keys {
			if k == y {
				return keys, i, nil
			}
		}
		return nil, 0, fmt.Errorf("choice label %q is not an option", y)
	case "score":
		var levels []json.RawMessage
		if err := json.Unmarshal(q.Criteria, &levels); err != nil {
			return nil, 0, fmt.Errorf("score criteria: %w", err)
		}
		for i := range levels {
			keys = append(keys, fmt.Sprint(i))
		}
		if err := json.Unmarshal(q.Label, &label); err != nil || label < 0 || label >= len(levels) {
			return nil, 0, fmt.Errorf("score label %s out of range", q.Label)
		}
		return keys, label, nil
	}
	return nil, 0, fmt.Errorf("unknown question type %q", q.Type)
}

// orderedKeys returns a JSON object's keys in document order.
func orderedKeys(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytesReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("choice criteria must be an object")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// LoadSuite reads a built-in suite (downloading and verifying it on first use) or a local JSONL file.
func LoadSuite(nameOrPath string) ([]Record, *BuiltinSuite, error) {
	b, ok := Builtins[nameOrPath]
	if !ok {
		recs, err := loadFile(nameOrPath, "kev", "")
		if err != nil && os.IsNotExist(errors.Unwrap(err)) {
			return nil, nil, fmt.Errorf("suite %q: not a built-in suite (%s) or a readable file", nameOrPath, builtinNames())
		}
		return recs, nil, err
	}
	var all []Record
	for i, f := range b.Files {
		path := filepath.Join(paths.Home(), "bench", "suites", b.Name, fmt.Sprintf("%d-%s.jsonl", i, f.Tier))
		if err := fetch(b.Name, f, path); err != nil {
			return nil, nil, err
		}
		recs, err := loadFile(path, b.Format, f.Tier)
		if err != nil {
			return nil, nil, err
		}
		all = append(all, recs...)
	}
	return all, &b, nil
}

func loadFile(path, format, tier string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 16<<20), 16<<20)
	for line := 1; sc.Scan(); line++ {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var r Record
		var err error
		if format == "jevbench" {
			r, err = fromJevBench(sc.Bytes())
		} else {
			err = json.Unmarshal(sc.Bytes(), &r)
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if r.Meta.ID == "" {
			r.Meta.ID = fmt.Sprintf("line-%d", line)
		}
		if r.Meta.Variant == "" {
			r.Meta.Variant = "clean"
		}
		if tier != "" {
			r.Meta.Tier = tier
		}
		for id, q := range r.Questions {
			if _, _, err := q.Keys(); err != nil {
				return nil, fmt.Errorf("%s:%d: question %s: %w", path, line, id, err)
			}
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

// fromJevBench converts one JevBench item ({id, family, state, question, expected})
// into a record: one question, labelled with expected ("yes"/"no" for noul, an
// option key for choice, a level index for score).
func fromJevBench(line []byte) (Record, error) {
	var item struct {
		ID       string          `json:"id"`
		Family   string          `json:"family"`
		State    json.RawMessage `json:"state"`
		Question struct {
			Type         string          `json:"type"`
			Instructions json.RawMessage `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"question"`
		Expected json.RawMessage `json:"expected"`
	}
	var r Record
	if err := json.Unmarshal(line, &item); err != nil {
		return r, err
	}
	label := item.Expected
	if item.Question.Type == "noul" {
		var yn string
		if err := json.Unmarshal(item.Expected, &yn); err != nil || (yn != "yes" && yn != "no") {
			return r, fmt.Errorf("noul expected must be \"yes\" or \"no\", got %s", item.Expected)
		}
		label, _ = json.Marshal(yn == "yes")
	}
	r.State = item.State
	r.Questions = map[string]LabelledQuery{"q": {
		Type: item.Question.Type, Instructions: item.Question.Instructions, Criteria: item.Question.Criteria,
		Label: label, Task: item.Family,
	}}
	r.Meta.ID = item.ID
	return r, nil
}

func fetch(suite string, f SuiteFile, path string) error {
	if data, err := os.ReadFile(path); err == nil && sha(data) == f.SHA256 {
		return nil
	}
	resp, err := http.Get(f.URL)
	if err != nil {
		return fmt.Errorf("downloading suite %s: %w", suite, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading suite %s: HTTP %d from %s", suite, resp.StatusCode, f.URL)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if got := sha(data); got != f.SHA256 {
		return fmt.Errorf("suite %s: %s has sha256 %s, not the pinned %s", suite, f.URL, got, f.SHA256)
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	return os.WriteFile(path, data, 0o644)
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func builtinNames() string {
	var names []string
	for n := range Builtins {
		names = append(names, n)
	}
	return fmt.Sprint(names)
}
