package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestAdminNeedsToken(t *testing.T) {
	srv := newTestServer(t, nil, false)
	resp, _ := http.Get(srv.URL + "/admin/status")
	if resp.StatusCode != 401 {
		t.Errorf("without the token: %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/admin/status", nil)
	req.Header.Set("X-Cyprid-Token", testToken)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Errorf("with the token: %d", resp.StatusCode)
	}
	// The question API stays open locally, like Ollama's.
	if resp, _ := http.Get(srv.URL + "/v1/models"); resp.StatusCode != 200 {
		t.Errorf("/v1/models: %d", resp.StatusCode)
	}
}

func TestBrowserSignIn(t *testing.T) {
	srv := newTestServer(t, nil, false)
	// Ask for a link the way `cyprid ui` does.
	req, _ := http.NewRequest("POST", srv.URL+"/admin/login-link", nil)
	req.Header.Set("X-Cyprid-Token", testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login-link: %v %v", err, resp)
	}
	var body struct{ URL string }
	decodeJSON(t, resp, &body)
	u, _ := url.Parse(body.URL)
	code := u.Query().Get("login")

	c := noRedirect()
	resp, _ = c.Get(srv.URL + "/?login=" + code)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Errorf("sign-in should redirect to /: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookie := resp.Header.Get("Set-Cookie")
	if !strings.Contains(cookie, "cyprid_admin="+testToken) || !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Errorf("cookie %q", cookie)
	}
	// The cookie lets the browser use the admin API.
	req, _ = http.NewRequest("GET", srv.URL+"/admin/status", nil)
	req.Header.Set("Cookie", strings.SplitN(cookie, ";", 2)[0])
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Errorf("with the cookie: %d", resp.StatusCode)
	}
	// The same link again, or a made-up one, signs nobody in.
	for _, code := range []string{code, "made-up"} {
		resp, _ = c.Get(srv.URL + "/?login=" + code)
		if resp.Header.Get("Set-Cookie") != "" {
			t.Errorf("code %q should not sign in", code)
		}
	}
}

func TestLoginLinkExpiry(t *testing.T) {
	s := &Server{cfg: Config{Addr: "127.0.0.1:1", AdminToken: "x"}}
	u, _ := url.Parse(s.LoginLink(-time.Second))
	if s.links.Use(u.Query().Get("login")) {
		t.Error("an expired link must not work")
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}
