package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cyprid/internal/recipe"
	"cyprid/internal/supervisor"
)

// newTestServer points CYPRID_HOME at a temp dir holding a user recipe
// for a fake remote Jev upstream, so requests go through the real proxy path.
func newTestServer(t *testing.T, upstream http.HandlerFunc, strict bool) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	home := t.TempDir()
	t.Setenv("CYPRID_HOME", home)
	os.MkdirAll(filepath.Join(home, "recipes"), 0o755)
	fake := `id: fake
remote:
  url: ` + up.URL + `
capabilities:
  score: emulated
  noul: unsupported
models:
  fake-model:
    upstream_model: fake-latest
    aliases: [fake]
`
	os.WriteFile(filepath.Join(home, "recipes", "fake.yaml"), []byte(fake), 0o644)

	c, err := recipe.Load()
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(supervisor.Config{})
	t.Cleanup(sup.StopAll)
	srv := httptest.NewServer(New(Config{AdminToken: testToken, StartWait: time.Second, Strict: strict}, c, sup).Handler())
	t.Cleanup(srv.Close)
	return srv
}

const testToken = "cyadm-test"

func post(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/v1/systemone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestProxiesToUpstreamWithRewrittenModel(t *testing.T) {
	var got map[string]any
	var raw []byte
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"model":"fake-1.0","answers":{"d":{"type":"choice","choice":"a","probabilities":{"a":1},"confidence":1}},"usage":{"input_tokens":3,"output_tokens":0}}`))
	}, false)

	status, out := post(t, srv.URL, `{"model":"fake","state":"hi","questions":{"d":{"type":"choice","instructions":"?","criteria":{"zebra":null,"apple":null}}},"x_strict":true}`)
	if status != 200 {
		t.Fatalf("status %d: %v", status, out)
	}
	if got["model"] != "fake-latest" {
		t.Errorf("upstream saw model %v, want fake-latest", got["model"])
	}
	if _, leaked := got["x_strict"]; leaked {
		t.Error("x_ extension fields must not reach the backend")
	}
	if !strings.Contains(string(raw), `{"zebra":null,"apple":null}`) {
		t.Errorf("choice options must reach the backend in the client's order, got %s", raw)
	}
	if out["model"] != "fake-1.0" {
		t.Errorf("remote responses keep the upstream's model name, got %v", out["model"])
	}
}

func TestCapabilities(t *testing.T) {
	calls := 0
	ok := func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"answers":{},"usage":{}}`))
	}
	score := `{"model":"fake-model","state":"s","questions":{"q":{"type":"score","instructions":"?","criteria":["a","b"]}}`
	noul := `{"model":"fake-model","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`

	lenient := newTestServer(t, ok, false)
	if status, out := post(t, lenient.URL, score+`}`); status != 200 {
		t.Errorf("emulated type should be allowed by default: %d %v", status, out)
	}
	if status, _ := post(t, lenient.URL, noul); status != 422 {
		t.Errorf("unsupported type: got %d, want 422", status)
	}

	strict := newTestServer(t, ok, true)
	if status, _ := post(t, strict.URL, score+`}`); status != 422 {
		t.Errorf("strict server should reject emulated type, got %d", status)
	}
	if status, _ := post(t, strict.URL, score+`,"x_strict":false}`); status != 200 {
		t.Errorf("x_strict:false should override strict server, got %d", status)
	}
}

func TestErrors(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		w.Write([]byte("overloaded"))
	}, false)

	cases := []struct {
		body   string
		status int
	}{
		{`not json`, 422},
		{`{"model":"nope","state":"s","questions":{"q":{"type":"choice"}}}`, 404},
		{`{"model":"fake","questions":{"q":{"type":"choice"}}}`, 422},
		{`{"model":"fake","state":"s","questions":{}}`, 422},
		{`{"model":"fake","state":"s","questions":{"q":{"type":"essay"}}}`, 422},
		{`{"model":"fake","state":"s","questions":{"q":{"type":"choice"}}}`, 529}, // upstream status passes through
	}
	for _, c := range cases {
		status, out := post(t, srv.URL, c.body)
		if status != c.status {
			t.Errorf("%s: status %d, want %d (%v)", c.body, status, c.status, out)
		}
		if _, ok := out["detail"]; !ok {
			t.Errorf("%s: error body should have detail, got %v", c.body, out)
		}
	}
}

func TestLocalModelNotInstalled(t *testing.T) {
	srv := newTestServer(t, nil, false)
	status, out := post(t, srv.URL, `{"model":"laya","state":"s","questions":{"q":{"type":"noul","instructions":"?"}}}`)
	if status != 409 || !strings.Contains(out["detail"].(string), "cyprid install laya") {
		t.Errorf("got %d %v", status, out)
	}
}
