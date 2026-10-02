package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestTokenFileIsPrivate(t *testing.T) {
	t.Setenv("CYPRID_HOME", t.TempDir())
	tok, err := LoadOrCreate()
	if err != nil || len(tok) < 40 {
		t.Fatalf("token %q err %v", tok, err)
	}
	info, _ := os.Stat(TokenPath())
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", info.Mode().Perm())
	}
	again, _ := LoadOrCreate()
	if again != tok {
		t.Error("the token must stay the same across starts")
	}
	os.Chmod(TokenPath(), 0o644)
	Load()
	if info, _ := os.Stat(TokenPath()); info.Mode().Perm() != 0o600 {
		t.Error("Load should repair widened permissions")
	}
}

func TestValid(t *testing.T) {
	r := httptest.NewRequest("GET", "/admin/status", nil)
	if Valid(r, "secret") {
		t.Error("no credentials must fail")
	}
	r.Header.Set(Header, "wrong")
	if Valid(r, "secret") {
		t.Error("a wrong token must fail")
	}
	r.Header.Set(Header, "secret")
	if !Valid(r, "secret") {
		t.Error("the header should work")
	}
	c := httptest.NewRequest("GET", "/admin/status", nil)
	c.AddCookie(&http.Cookie{Name: Cookie, Value: "secret"})
	if !Valid(c, "secret") {
		t.Error("the cookie should work")
	}
	if Valid(httptest.NewRequest("GET", "/", nil), "") {
		t.Error("an empty server token must never let anyone in")
	}
}

func TestLinksAreOneTimeAndExpire(t *testing.T) {
	var l Links
	code := l.New(time.Minute)
	if !l.Use(code) || l.Use(code) {
		t.Error("a code must work exactly once")
	}
	old := l.New(-time.Second)
	if l.Use(old) {
		t.Error("an expired code must fail")
	}
	if l.Use("made-up") {
		t.Error("an unknown code must fail")
	}
}
