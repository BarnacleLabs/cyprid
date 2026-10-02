package server

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"cyprid/internal/recipe"
)

// normalise makes backend responses match Jev's wire format. It only fixes
// shape, never probability values:
//
//   - local backends report the model name the client asked for (Kev, for
//     example, echoes "kev-latest")
//   - score probabilities keyed by level text (jevmlx) are re-keyed by level
//     index, as Jev does, and legend is filled in
//   - options a backend left out (jevmlx reports only its top 5) are listed
//     with probability 0, and the answer is marked "x_truncated": true
//   - usage reported as prompt_tokens becomes input_tokens
//   - non-JSON errors are wrapped as {"detail": ...}
//
// It returns the new body and a list of what it changed, for the
// X-Cyprid-Normalised header; answers that already match are left
// byte for byte.
func normalise(ref recipe.Ref, status int, body []byte, questions json.RawMessage) ([]byte, []string) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		out, _ := json.Marshal(map[string]string{"detail": strings.TrimSpace(string(body))})
		return out, nil
	}
	if status >= 300 {
		return body, nil
	}
	var changed []string
	if !ref.IsRemote() {
		obj["model"], _ = json.Marshal(ref.Name)
	}
	if u, ok := normaliseUsage(obj["usage"]); ok {
		obj["usage"] = u
		changed = append(changed, "usage")
	}

	var qs map[string]json.RawMessage
	var answers map[string]json.RawMessage
	if json.Unmarshal(questions, &qs) == nil && json.Unmarshal(obj["answers"], &answers) == nil {
		touched := false
		for id, raw := range answers {
			if fixed, what := normaliseAnswer(qs[id], raw); what != "" {
				answers[id] = fixed
				touched = true
				changed = appendOnce(changed, what)
			}
		}
		if touched {
			obj["answers"], _ = json.Marshal(answers)
		}
	}
	out, _ := json.Marshal(obj)
	return out, changed
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func normaliseUsage(raw json.RawMessage) (json.RawMessage, bool) {
	var u map[string]json.RawMessage
	if json.Unmarshal(raw, &u) != nil {
		return nil, false
	}
	if _, ok := u["input_tokens"]; ok {
		return nil, false
	}
	prompt, ok := u["prompt_tokens"]
	if !ok {
		return nil, false
	}
	u["input_tokens"] = prompt
	if _, ok := u["output_tokens"]; !ok {
		u["output_tokens"] = json.RawMessage("0")
	}
	out, _ := json.Marshal(u)
	return out, true
}

// optionKeys is the ordered outcome space of a question: choice criteria keys,
// or score level texts (rendered as a backend would compare them).
func optionKeys(q json.RawMessage) (qtype string, keys []string) {
	var head struct {
		Type     string          `json:"type"`
		Criteria json.RawMessage `json:"criteria"`
	}
	if json.Unmarshal(q, &head) != nil {
		return "", nil
	}
	switch head.Type {
	case "choice":
		keys, _ = objectKeys(head.Criteria)
	case "score":
		var levels []json.RawMessage
		json.Unmarshal(head.Criteria, &levels)
		for _, l := range levels {
			var s string
			if json.Unmarshal(l, &s) != nil {
				s = string(l) // structured level: compare by its JSON
			}
			keys = append(keys, s)
		}
	}
	return head.Type, keys
}

func objectKeys(raw json.RawMessage) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, err
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

// normaliseAnswer returns the fixed answer and a label for what changed, or "" if it was fine.
func normaliseAnswer(q, raw json.RawMessage) (json.RawMessage, string) {
	qtype, keys := optionKeys(q)
	if len(keys) == 0 {
		return nil, ""
	}
	var a map[string]json.RawMessage
	if json.Unmarshal(raw, &a) != nil {
		return nil, ""
	}
	var probs map[string]float64
	if json.Unmarshal(a["probabilities"], &probs) != nil || probs == nil {
		return nil, ""
	}

	// Target keys: choice option names; score level indices "0".."n-1".
	target := keys
	what := ""
	if qtype == "score" {
		target = make([]string, len(keys))
		for i := range keys {
			target[i] = strconv.Itoa(i)
		}
		byText := map[string]string{}
		for i, k := range keys {
			byText[k] = strconv.Itoa(i)
		}
		remapped := map[string]float64{}
		for k, v := range probs {
			if _, err := strconv.Atoi(k); err == nil {
				remapped[k] = v
			} else if idx, ok := byText[k]; ok {
				remapped[idx] = v
				what = "score-keys"
			} else {
				return nil, "" // unrecognised keys: leave the answer alone
			}
		}
		probs = remapped
		if _, ok := a["legend"]; !ok || what != "" {
			legend := orderedObject(target, func(i int) any { return keys[i] })
			a["legend"] = legend
		}
	} else if qtype != "choice" {
		return nil, ""
	}

	missing, total := 0, 0.0
	for _, k := range target {
		if v, ok := probs[k]; ok {
			total += v
		} else {
			missing++
		}
	}
	if missing == 0 && what == "" {
		return nil, ""
	}
	if missing > 0 {
		if what == "" {
			what = "missing-options"
		}
		if total < 0.99 {
			a["x_truncated"] = json.RawMessage("true")
		}
	}
	a["probabilities"] = orderedObject(target, func(i int) any { return probs[target[i]] })
	out, _ := json.Marshal(a)
	return out, what
}

// orderedObject builds a JSON object with keys in the given order (Go maps would sort them).
func orderedObject(keys []string, value func(i int) any) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := json.Marshal(k)
		vj, _ := json.Marshal(value(i))
		b.Write(kj)
		b.WriteByte(':')
		b.Write(vj)
	}
	b.WriteByte('}')
	return b.Bytes()
}
