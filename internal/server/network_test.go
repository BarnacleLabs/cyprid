package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"cyprid/internal/recipe"
	"cyprid/internal/supervisor"
)

func TestAPIAuth(t *testing.T) {
	s := &Server{}
	s.net.settings.APIKey = "ds-secret"
	var sawAuth string
	h := s.apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	cases := []struct {
		name, remote, auth string
		want               int
		forwarded          string
	}{
		{"remote without key", "192.168.1.9:4000", "", 401, ""},
		{"remote with wrong key", "192.168.1.9:4000", "Bearer nope", 401, ""},
		{"remote with key; not forwarded", "192.168.1.9:4000", "Bearer ds-secret", 200, ""},
		{"local, no key needed", "127.0.0.1:4000", "", 200, ""},
		{"local TypeSafe key passes through", "127.0.0.1:4000", "Bearer ts-live-123", 200, "Bearer ts-live-123"},
		{"local with our key; not forwarded", "127.0.0.1:4000", "Bearer ds-secret", 200, ""},
	}
	for _, c := range cases {
		sawAuth = "unset"
		req := httptest.NewRequest("POST", "/v1/systemone", nil)
		req.RemoteAddr = c.remote
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want == 200 && sawAuth != c.forwarded {
			t.Errorf("%s: backend saw Authorization %q, want %q", c.name, sawAuth, c.forwarded)
		}
	}
}

// lanIP returns a non-loopback IPv4 address of this machine, or "" if there is none.
func lanIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

func TestNetworkToggle(t *testing.T) {
	ip := lanIP()
	if ip == "" {
		t.Skip("no network interface")
	}
	t.Setenv("CYPRID_HOME", t.TempDir())
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	c, err := recipe.Load()
	if err != nil {
		t.Fatal(err)
	}
	sup := supervisor.New(supervisor.Config{})
	defer sup.StopAll()
	s := New(Config{}, c, sup)
	if err := s.Start("127.0.0.1:" + port); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown(t.Context())

	client := &http.Client{Timeout: 2 * time.Second}
	get := func(host string) (int, error) {
		resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, nil
	}

	if _, err := get(ip); err == nil {
		t.Fatal("network address reachable before network mode was turned on")
	}
	if err := s.SetNetwork(true); err != nil {
		t.Fatal(err)
	}
	if code, err := get(ip); err != nil || code != 200 {
		t.Fatalf("network on: %d %v", code, err)
	}
	if s.apiKey() == "" {
		t.Error("turning network mode on should create an API key")
	}
	// Models listing from the network address needs the key; this machine's LAN IP isn't loopback.
	resp, err := client.Get("http://" + net.JoinHostPort(ip, port) + "/v1/models")
	if err != nil || resp.StatusCode != 401 {
		t.Errorf("unauthenticated network request: %v %v", resp.StatusCode, err)
	}
	if err := s.SetNetwork(false); err != nil {
		t.Fatal(err)
	}
	if _, err := get(ip); err == nil {
		t.Error("network address still reachable after network mode was turned off")
	}
	if code, err := get("127.0.0.1"); err != nil || code != 200 {
		t.Errorf("localhost must keep working: %d %v", code, err)
	}
	// The setting persists for the next start.
	s2 := New(Config{}, c, sup)
	s.SetNetwork(true)
	s.Shutdown(t.Context())
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	port2 := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if err := s2.Start("127.0.0.1:" + strconv.Itoa(port2)); err != nil {
		t.Fatal(err)
	}
	defer s2.Shutdown(t.Context())
	if !s2.networkInfo().Listening {
		t.Error("network mode should be restored on start")
	}
}
