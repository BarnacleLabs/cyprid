package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"cyprid/internal/recipe"
)

func TestLocalOnlyBlocksRebindingAndCSRF(t *testing.T) {
	h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	cases := []struct {
		name, host, origin, site string
		want                     int
		remote                   string
	}{
		{"from another machine", "localhost:8765", "", "", 403, "192.168.1.50:5000"},
		{"cli", "127.0.0.1:8765", "", "", 200, ""},
		{"ui", "localhost:8765", "http://localhost:8765", "same-origin", 200, ""},
		{"ipv6", "[::1]:8765", "", "", 200, "[::1]:5000"},
		{"dns rebinding", "evil.example:8765", "", "", 403, ""},
		{"cross-origin page", "127.0.0.1:8765", "https://evil.example", "", 403, ""},
		{"other local port", "127.0.0.1:8765", "http://127.0.0.1:3000", "", 403, ""},
		{"cross-site fetch", "127.0.0.1:8765", "", "cross-site", 403, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/admin/recipes/x", nil)
		req.Host = c.host
		req.RemoteAddr = "127.0.0.1:5000"
		if c.remote != "" {
			req.RemoteAddr = c.remote
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestSaveRecipeValidatesAndRollsBack(t *testing.T) {
	srv := newTestServer(t, nil, false)
	put := func(id, yaml string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"yaml": yaml})
		req, _ := http.NewRequest("PUT", srv.URL+"/admin/recipes/"+id, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Cyprid-Token", testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, _ := put("mine", "id: mine\nremote: {url: http://127.0.0.1:1}\nmodels: {mine-model: {}}\n"); code != 200 {
		t.Fatalf("valid recipe: %d", code)
	}
	if code, _ := put("mine", "id: mine\nnot_a_field: 1\n"); code != 422 {
		t.Errorf("invalid yaml should be rejected, got %d", code)
	}
	if code, _ := put("other", "id: mine\nremote: {url: u}\nmodels: {m: {}}\n"); code != 422 {
		t.Errorf("id mismatch should be rejected, got %d", code)
	}
	// Parses on its own but clashes with the built-in laya model: must be rejected and not left on disk.
	if code, out := put("clash", "id: clash\nremote: {url: u}\nmodels: {laya: {}}\n"); code != 422 {
		t.Errorf("clashing model name should be rejected, got %d %v", code, out)
	}
	if _, err := os.Stat(recipe.UserPath("clash")); !os.IsNotExist(err) {
		t.Error("rejected recipe was left on disk")
	}
	if code, _ := put("../escape", "id: x"); code != 400 && code != 404 {
		t.Errorf("path traversal id: got %d", code)
	}
	if c, _ := recipe.Load(); c != nil {
		if _, ok := c.Resolve("mine-model"); !ok {
			t.Error("the valid recipe should still be loaded")
		}
	}
}

func TestUIServed(t *testing.T) {
	srv := newTestServer(t, nil, false)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
}
