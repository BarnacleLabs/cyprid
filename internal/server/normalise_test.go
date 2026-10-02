package server

import (
	"encoding/json"
	"strings"
	"testing"

	"cyprid/internal/recipe"
)

var localRef = recipe.Ref{Name: "m", Recipe: &recipe.Recipe{ID: "r"}}

const questionsJSON = `{
  "lvl": {"type": "score", "instructions": "?", "criteria": ["Calm", "Frustrated", "Very angry"]},
  "pick": {"type": "choice", "instructions": "?", "criteria": {"z": null, "a": null, "m": null}}
}`

func TestNormaliseJevmlxShapes(t *testing.T) {
	body := `{"model":"x","answers":{
	  "lvl": {"type":"score","score":1.2,"probabilities":{"Calm":0.1,"Frustrated":0.6,"Very angry":0.3}},
	  "pick":{"type":"choice","choice":"a","probabilities":{"a":0.7,"z":0.2}}
	},"usage":{"prompt_tokens":42}}`
	out, changed := normalise(localRef, 200, []byte(body), json.RawMessage(questionsJSON))

	var resp struct {
		Model   string
		Answers map[string]map[string]json.RawMessage
		Usage   map[string]int
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "m" {
		t.Errorf("model %q", resp.Model)
	}
	if got := string(resp.Answers["lvl"]["probabilities"]); got != `{"0":0.1,"1":0.6,"2":0.3}` {
		t.Errorf("score probabilities %s", got)
	}
	if got := string(resp.Answers["lvl"]["legend"]); got != `{"0":"Calm","1":"Frustrated","2":"Very angry"}` {
		t.Errorf("legend %s", got)
	}
	if got := string(resp.Answers["pick"]["probabilities"]); got != `{"z":0.2,"a":0.7,"m":0}` {
		t.Errorf("choice probabilities should list every option in criteria order: %s", got)
	}
	if string(resp.Answers["pick"]["x_truncated"]) != "true" {
		t.Error("an answer missing probability mass should be marked x_truncated")
	}
	if resp.Usage["input_tokens"] != 42 {
		t.Errorf("usage %v", resp.Usage)
	}
	if strings.Join(changed, ",") != "usage,score-keys,missing-options" && strings.Join(changed, ",") != "usage,missing-options,score-keys" {
		t.Errorf("changed = %v", changed)
	}
}

func TestNormaliseLeavesJevShapedAnswersAlone(t *testing.T) {
	answers := `{"lvl":{"type":"score","score":1.05,"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},"probabilities":{"0":0.0,"1":0.95,"2":0.05},"confidence":0.92},` +
		`"pick":{"type":"choice","choice":"a","probabilities":{"z":0.1,"a":0.8,"m":0.1},"confidence":0.7}}`
	body := `{"model":"jev-1.13.0","answers":` + answers + `,"usage":{"input_tokens":3,"output_tokens":1}}`
	remote := recipe.Ref{Name: "jev", Recipe: &recipe.Recipe{ID: "jev", Remote: &recipe.Remote{URL: "u"}}}
	out, changed := normalise(remote, 200, []byte(body), json.RawMessage(questionsJSON))
	if len(changed) != 0 {
		t.Errorf("nothing should change, got %v", changed)
	}
	var resp map[string]json.RawMessage
	json.Unmarshal(out, &resp)
	if string(resp["answers"]) != answers {
		t.Errorf("answers must pass through byte for byte:\n got %s\nwant %s", resp["answers"], answers)
	}
	if string(resp["model"]) != `"jev-1.13.0"` {
		t.Error("remote model name must be kept")
	}
}
