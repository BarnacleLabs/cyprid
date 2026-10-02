package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"cyprid/internal/config"
)

// Listeners: the server always listens on localhost. With network mode on it
// also listens on all interfaces, on the same port, so it can be switched on
// and off at runtime without disturbing local clients. The setting persists in
// ~/.cyprid/config.json.
//
// Clients on other machines must send the server's API key as a bearer token.
// The admin API and the UI stay local-only either way (see localOnly).
type network struct {
	mu       sync.Mutex
	httpSrv  *http.Server
	host     string
	port     string
	wide     net.Listener // all-interfaces listener while network mode is on
	settings config.Settings
	lastErr  string
}

// Start listens on addr (which must be a loopback address) and applies the saved network setting.
func (s *Server) Start(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("--addr must be a localhost address (got %s); use network mode to listen on the network", host)
	}
	settings, err := config.Load()
	if err != nil {
		return fmt.Errorf("reading %s: %w", "config.json", err)
	}
	loop, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	n := &s.net
	n.mu.Lock()
	n.httpSrv = &http.Server{Handler: s.Handler()}
	n.host, n.port, n.settings = host, port, settings
	n.mu.Unlock()
	go func() {
		if err := n.httpSrv.Serve(loop); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()
	if settings.Network {
		if err := s.SetNetwork(true); err != nil {
			log.Printf("network mode is on but the network listener failed: %v (serving on localhost only)", err)
		}
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.net.mu.Lock()
	srv := s.net.httpSrv
	s.net.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// SetNetwork turns listening on all interfaces on or off, and saves the setting.
func (s *Server) SetNetwork(on bool) error {
	n := &s.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.httpSrv == nil {
		return errors.New("server not started")
	}
	n.lastErr = ""
	if on && n.wide == nil {
		if n.settings.APIKey == "" {
			n.settings.APIKey = config.NewAPIKey()
		}
		l, err := net.Listen("tcp", net.JoinHostPort("", n.port))
		if err != nil {
			n.lastErr = err.Error()
			return fmt.Errorf("listening on the network: %w", err)
		}
		n.wide = l
		go func() {
			if err := n.httpSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				log.Printf("network listener: %v", err)
			}
		}()
		log.Printf("network mode on: listening on all interfaces, port %s (clients need the API key)", n.port)
	}
	if !on && n.wide != nil {
		// Closing the listener refuses new network connections; requests already in flight finish.
		n.wide.Close()
		n.wide = nil
		log.Printf("network mode off: localhost only")
	}
	n.settings.Network = on
	return config.Save(n.settings)
}

func (s *Server) RotateAPIKey() (string, error) {
	n := &s.net
	n.mu.Lock()
	defer n.mu.Unlock()
	n.settings.APIKey = config.NewAPIKey()
	return n.settings.APIKey, config.Save(n.settings)
}

func (s *Server) apiKey() string {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	return s.net.settings.APIKey
}

// apiAuth requires the API key from clients that aren't on this machine. A
// request carrying the key has the header removed, so it isn't forwarded to a
// remote backend such as Jev (which gets the server's own TypeSafe key instead).
func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := s.apiKey()
		token, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key != "" && hasBearer && subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
			r.Header.Del("Authorization")
			next.ServeHTTP(w, r)
			return
		}
		if !isLoopbackRemote(r.RemoteAddr) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cyprid"`)
			fail(w, http.StatusUnauthorized, "requests from other machines need Authorization: Bearer <cyprid API key> "+
				"(shown in the web UI on the server, or run: cyprid network)")
			return
		}
		next.ServeHTTP(w, r) // local: any Authorization header is the client's own, e.g. a TypeSafe key for jev
	})
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type networkInfo struct {
	Enabled   bool     `json:"enabled"`
	Listening bool     `json:"listening"`
	Port      string   `json:"port"`
	URLs      []string `json:"urls"`
	APIKey    string   `json:"api_key,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func (s *Server) networkInfo() networkInfo {
	n := &s.net
	n.mu.Lock()
	defer n.mu.Unlock()
	info := networkInfo{Enabled: n.settings.Network, Listening: n.wide != nil, Port: n.port, APIKey: n.settings.APIKey, Error: n.lastErr}
	info.URLs = lanURLs(n.port)
	return info
}

// lanURLs lists the addresses other machines can reach this one on.
func lanURLs(port string) []string {
	var urls []string
	if h, err := os.Hostname(); err == nil && h != "" {
		if !strings.Contains(h, ".") {
			h += ".local"
		}
		urls = append(urls, "http://"+net.JoinHostPort(h, port))
	}
	ifaces, _ := net.Interfaces()
	var ips []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipn.IP.String())
			}
		}
	}
	sort.Strings(ips)
	for _, ip := range ips {
		urls = append(urls, "http://"+net.JoinHostPort(ip, port))
	}
	return urls
}

func (s *Server) adminNetwork(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.networkInfo())
}

func (s *Server) adminSetNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Enabled == nil {
		fail(w, http.StatusBadRequest, `body must be {"enabled": true|false}`)
		return
	}
	if err := s.SetNetwork(*body.Enabled); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.networkInfo())
}

func (s *Server) adminRotateKey(w http.ResponseWriter, r *http.Request) {
	if _, err := s.RotateAPIKey(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.networkInfo())
}
