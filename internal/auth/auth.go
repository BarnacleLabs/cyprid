// Package auth protects cyprid's admin API and web page with a per-user access
// token. The admin API can install software and edit recipes (which decide what
// commands run), so on a Mac shared by several accounts only the account that
// runs cyprid may use it.
//
// The token lives in ~/.cyprid/admin-token, readable only by its owner. The
// cyprid command sends it in a header. Browsers sign in with a one-time link
// (from `cyprid serve` or `cyprid ui`) that is exchanged for a cookie, so the
// token itself never appears in an address or the browser's history.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cyprid/internal/paths"
)

const (
	Header = "X-Cyprid-Token" // how the cyprid command sends the token
	Cookie = "cyprid_admin"   // how a signed-in browser sends it
)

func TokenPath() string { return filepath.Join(paths.Home(), "admin-token") }

func random(prefix string) string {
	b := make([]byte, 24)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// LoadOrCreate returns the token, creating it (owner-only) on first use.
func LoadOrCreate() (string, error) {
	if t, err := Load(); err == nil {
		return t, nil
	}
	if err := os.MkdirAll(paths.Home(), 0o755); err != nil {
		return "", err
	}
	t := random("cyadm-")
	if err := os.WriteFile(TokenPath(), []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	return t, nil
}

// Load reads the token, and makes sure the file is readable only by its owner.
func Load() (string, error) {
	data, err := os.ReadFile(TokenPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("no admin token yet (%s); start the server once with: cyprid serve", TokenPath())
	}
	if err != nil {
		return "", err
	}
	os.Chmod(TokenPath(), 0o600) // repair it if something widened the permissions
	t := strings.TrimSpace(string(data))
	if t == "" {
		return "", fmt.Errorf("%s is empty; delete it and restart cyprid serve", TokenPath())
	}
	return t, nil
}

// Valid reports whether a request carries the token, in the header or the cookie.
func Valid(r *http.Request, token string) bool {
	if token == "" {
		return false // never open by accident
	}
	candidates := []string{r.Header.Get(Header)}
	if c, err := r.Cookie(Cookie); err == nil {
		candidates = append(candidates, c.Value)
	}
	for _, c := range candidates {
		if c != "" && subtle.ConstantTimeCompare([]byte(c), []byte(token)) == 1 {
			return true
		}
	}
	return false
}

// SetCookie signs a browser in. HttpOnly keeps page scripts from reading it;
// SameSite=Strict means other sites' pages never send it.
func SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: Cookie, Value: token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
}

// Links hands out one-time sign-in codes for the web page.
type Links struct {
	mu    sync.Mutex
	codes map[string]time.Time // code -> expiry
}

// New returns a code that signs a browser in once, within ttl.
func (l *Links) New(ttl time.Duration) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.codes == nil {
		l.codes = map[string]time.Time{}
	}
	now := time.Now()
	for c, exp := range l.codes {
		if now.After(exp) {
			delete(l.codes, c)
		}
	}
	code := random("")
	l.codes[code] = now.Add(ttl)
	return code
}

// Use consumes a code, reporting whether it was valid.
func (l *Links) Use(code string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	exp, ok := l.codes[code]
	if !ok {
		return false
	}
	delete(l.codes, code) // single use
	return time.Now().Before(exp)
}
