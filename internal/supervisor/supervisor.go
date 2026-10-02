// Package supervisor runs one worker process per local model: it starts a model
// on first use, waits until it is really ready, stops it when idle, keeps the
// total within a memory budget, and captures its logs.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"cyprid/internal/install"
	"cyprid/internal/paths"
	"cyprid/internal/recipe"
)

type State string

const (
	Stopped  State = "stopped"
	Starting State = "starting"
	Ready    State = "ready"
	Stopping State = "stopping"
	Failed   State = "failed"
)

var (
	ErrNotInstalled = errors.New("not installed")
	ErrStillLoading = errors.New("still loading")
)

// StartError means the worker failed to start or crashed; Logs holds its last output.
type StartError struct {
	Model string
	Err   error
	Logs  []string
	Retry time.Duration // how long until another start will be attempted
}

func (e *StartError) Error() string { return fmt.Sprintf("%s failed: %v", e.Model, e.Err) }

// MemoryError means the model can't fit in the memory budget, even after stopping idle models.
type MemoryError struct {
	Model            string
	NeedGB, BudgetGB float64
	BusyGB           float64
}

func (e *MemoryError) Error() string {
	return fmt.Sprintf("%s needs ~%.0fGB but only %.0fGB of the %.0fGB budget is free (busy models can't be stopped); "+
		"stop a model or raise --max-mem-gb", e.Model, e.NeedGB, e.BudgetGB-e.BusyGB, e.BudgetGB)
}

type Config struct {
	MaxMemGB    float64
	StopTimeout time.Duration
}

type Supervisor struct {
	cfg   Config
	mu    sync.Mutex
	procs map[string]*proc
	quit  chan struct{}
}

type proc struct {
	ref       recipe.Ref
	state     State
	cmd       *exec.Cmd
	port      int
	readyCh   chan struct{} // closed when the process leaves Starting
	exited    chan struct{} // closed when the process exits
	err       error
	logs      *ring
	startedAt time.Time
	lastUsed  time.Time
	inflight  int
	failures  int
	failedAt  time.Time
}

func New(cfg Config) *Supervisor {
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	s := &Supervisor{cfg: cfg, procs: map[string]*proc{}, quit: make(chan struct{})}
	go s.reapIdle()
	return s
}

// Acquire returns the base URL of a ready worker for ref, starting it if needed.
// It waits up to wait for a cold start; if the model is still loading after
// that, it returns ErrStillLoading and the start carries on in the background.
// Callers must call release when their request is done.
func (s *Supervisor) Acquire(ctx context.Context, ref recipe.Ref, wait time.Duration) (baseURL string, release func(), err error) {
	s.mu.Lock()
	p := s.procs[ref.Name]
	if p == nil {
		p = &proc{ref: ref, state: Stopped, logs: newRing(500)}
		s.procs[ref.Name] = p
	}
	p.ref = ref // pick up recipe edits on the next start

	switch p.state {
	case Stopped, Failed:
		if err := s.startLocked(p); err != nil {
			s.mu.Unlock()
			return "", nil, err
		}
	case Stopping:
		exited := p.exited
		s.mu.Unlock()
		select {
		case <-exited:
		case <-ctx.Done():
			return "", nil, ctx.Err()
		}
		return s.Acquire(ctx, ref, wait)
	}
	readyCh := p.readyCh
	s.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-readyCh:
	case <-timer.C:
		return "", nil, ErrStillLoading
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p.state != Ready {
		err := p.err
		if err == nil {
			err = errors.New("stopped while starting")
		}
		return "", nil, &StartError{Model: ref.Name, Err: err, Logs: p.logs.tail(40)}
	}
	p.inflight++
	p.lastUsed = time.Now()
	url := fmt.Sprintf("http://127.0.0.1:%d", p.port)
	var once sync.Once
	return url, func() {
		once.Do(func() {
			s.mu.Lock()
			p.inflight--
			p.lastUsed = time.Now()
			s.mu.Unlock()
		})
	}, nil
}

// Start begins loading a model without waiting for it.
func (s *Supervisor) Start(ref recipe.Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[ref.Name]
	if p == nil {
		p = &proc{ref: ref, state: Stopped, logs: newRing(500)}
		s.procs[ref.Name] = p
	}
	p.ref = ref
	if p.state == Stopped || p.state == Failed {
		p.failures = 0 // an explicit start overrides crash backoff
		return s.startLocked(p)
	}
	return nil
}

// backoff is how long to wait before restarting after consecutive failures.
// The count resets once a worker becomes ready, so a model that crashes after
// running fine restarts at once; only repeated failures (a crash loop) back off.
func backoff(failures int) time.Duration {
	if failures <= 1 {
		return 0
	}
	return min(time.Second<<min(failures-1, 6), time.Minute) // 2s, 4s ... 60s
}

func (s *Supervisor) startLocked(p *proc) error {
	ref := p.ref
	if st := install.StatusOf(ref); st != install.Installed {
		return fmt.Errorf("%w (%s)", ErrNotInstalled, st)
	}
	if p.state == Failed {
		if wait := backoff(p.failures) - time.Since(p.failedAt); wait > 0 {
			return &StartError{Model: ref.Name, Err: p.err, Logs: p.logs.tail(40), Retry: wait}
		}
	}
	if err := s.makeRoomLocked(ref); err != nil {
		return err
	}
	if err := install.WriteShim(ref); err != nil {
		return err
	}

	port, err := freePort()
	if err != nil {
		return err
	}
	args := ref.LaunchArgs(port)
	cmd := exec.Command(args[0], args[1:]...)
	// Models run offline: everything they need was downloaded at install, so they use
	// exactly the installed versions and never fetch or update anything while running.
	// A recipe can set HF_HUB_OFFLINE: "0" if its backend truly needs the network.
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1")
	for k, v := range ref.Recipe.Launch.Env {
		cmd.Env = append(cmd.Env, k+"="+ref.Expand(v, port))
	}
	cmd.Dir = ref.EnvDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so stop reaches every child process

	os.MkdirAll(paths.Logs(), 0o755)
	logFile, err := os.OpenFile(filepath.Join(paths.Logs(), ref.Name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(logFile, "\n==== %s starting %s on port %d: %v\n", time.Now().Format(time.RFC3339), ref.Name, port, args)
	out := io.MultiWriter(logFile, p.logs)
	cmd.Stdout, cmd.Stderr = out, out

	p.logs.reset()
	if err := cmd.Start(); err != nil {
		logFile.Close()
		p.state, p.err, p.failedAt = Failed, err, time.Now()
		p.failures++
		return &StartError{Model: ref.Name, Err: err}
	}
	p.cmd, p.port, p.err = cmd, port, nil
	p.state = Starting
	p.startedAt = time.Now()
	p.lastUsed = time.Now()
	p.readyCh = make(chan struct{})
	p.exited = make(chan struct{})

	go s.watchExit(p, cmd, logFile)
	go s.waitReady(p, cmd)
	return nil
}

func (s *Supervisor) watchExit(p *proc, cmd *exec.Cmd, logFile *os.File) {
	err := cmd.Wait()
	logFile.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.cmd != cmd {
		return
	}
	switch p.state {
	case Stopping:
		p.state = Stopped
	case Failed:
		// already marked failed (readiness timeout) before it was killed
	default:
		if err == nil {
			err = errors.New("worker exited")
		}
		p.state, p.err, p.failedAt = Failed, fmt.Errorf("worker exited: %v", err), time.Now()
		p.failures++
	}
	p.cmd = nil
	closeOnce(p.readyCh)
	close(p.exited)
}

func (s *Supervisor) waitReady(p *proc, cmd *exec.Cmd) {
	ready := p.ref.Recipe.Launch.Ready
	url := fmt.Sprintf("http://127.0.0.1:%d%s", p.port, ready.Path)
	deadline := time.Now().Add(ready.Timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	probe := func() (*http.Response, error) { return client.Get(url) }
	if ready.Body != "" {
		// A real request loads the model, which can take a while, so allow longer than a health check.
		client.Timeout = 2 * time.Minute
		probe = func() (*http.Response, error) {
			return client.Post(url, "application/json", strings.NewReader(ready.Body))
		}
	}
	for {
		s.mu.Lock()
		if p.cmd != cmd || p.state != Starting {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		if resp, err := probe(); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				s.mu.Lock()
				if p.cmd == cmd && p.state == Starting {
					p.state, p.failures = Ready, 0
					closeOnce(p.readyCh)
				}
				s.mu.Unlock()
				return
			}
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			timedOut := p.cmd == cmd && p.state == Starting
			if timedOut {
				p.err = fmt.Errorf("not ready after %s (edit launch.ready.timeout in the recipe to wait longer)", ready.Timeout)
				p.state, p.failedAt = Failed, time.Now()
				p.failures++
				closeOnce(p.readyCh)
			}
			s.mu.Unlock()
			if timedOut {
				s.kill(p, cmd)
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// makeRoomLocked stops idle models, least recently used first, until ref fits the budget.
func (s *Supervisor) makeRoomLocked(ref recipe.Ref) error {
	need := ref.MemGB()
	if s.cfg.MaxMemGB <= 0 || need <= 0 {
		return nil
	}
	used := 0.0
	var idle []*proc
	for _, p := range s.procs {
		if p.ref.Name == ref.Name || (p.state != Starting && p.state != Ready) {
			continue
		}
		used += p.ref.MemGB()
		if p.state == Ready && p.inflight == 0 {
			idle = append(idle, p)
		}
	}
	if used+need <= s.cfg.MaxMemGB {
		return nil
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].lastUsed.Before(idle[j].lastUsed) })
	var evict []*proc
	for _, p := range idle {
		if used+need <= s.cfg.MaxMemGB {
			break
		}
		evict = append(evict, p)
		used -= p.ref.MemGB()
	}
	if used+need > s.cfg.MaxMemGB {
		return &MemoryError{Model: ref.Name, NeedGB: need, BudgetGB: s.cfg.MaxMemGB, BusyGB: used}
	}
	for _, p := range evict {
		p.logs.Write([]byte(fmt.Sprintf("cyprid: stopping to free memory for %s\n", ref.Name)))
		s.stopLocked(p)
	}
	// Wait (without the lock) is unnecessary here: stopped models no longer count, and
	// their memory is released as the processes exit.
	return nil
}

// Stop stops a model and waits for its process to exit.
func (s *Supervisor) Stop(name string) bool {
	s.mu.Lock()
	p := s.procs[name]
	if p == nil || (p.state != Starting && p.state != Ready) {
		s.mu.Unlock()
		return false
	}
	exited := s.stopLocked(p)
	s.mu.Unlock()
	<-exited
	return true
}

func (s *Supervisor) stopLocked(p *proc) <-chan struct{} {
	p.state = Stopping
	closeOnce(p.readyCh)
	cmd, exited := p.cmd, p.exited
	go s.kill(p, cmd)
	return exited
}

// kill sends SIGTERM to the worker's process group, then SIGKILL after StopTimeout.
func (s *Supervisor) kill(p *proc, cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	syscall.Kill(-pgid, syscall.SIGTERM)
	s.mu.Lock()
	exited := p.exited
	s.mu.Unlock()
	select {
	case <-exited:
	case <-time.After(s.cfg.StopTimeout):
		syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// StopAll stops every running model; used on shutdown.
func (s *Supervisor) StopAll() {
	close(s.quit)
	s.mu.Lock()
	var waits []<-chan struct{}
	for _, p := range s.procs {
		if p.state == Starting || p.state == Ready {
			waits = append(waits, s.stopLocked(p))
		}
	}
	s.mu.Unlock()
	for _, w := range waits {
		<-w
	}
}

func (s *Supervisor) reapIdle() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
		s.mu.Lock()
		for _, p := range s.procs {
			ttl := p.ref.Recipe.Launch.IdleTimeout
			if p.state == Ready && p.inflight == 0 && ttl > 0 && time.Since(p.lastUsed) > ttl {
				p.logs.Write([]byte(fmt.Sprintf("cyprid: idle for %s, stopping\n", ttl)))
				s.stopLocked(p)
			}
		}
		s.mu.Unlock()
	}
}

type Status struct {
	State    State      `json:"state"`
	Port     int        `json:"port,omitempty"`
	Since    *time.Time `json:"since,omitempty"`
	LastUsed *time.Time `json:"last_used,omitempty"`
	Inflight int        `json:"inflight,omitempty"`
	Error    string     `json:"error,omitempty"`
}

func (s *Supervisor) Status(name string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[name]
	if p == nil {
		return Status{State: Stopped}
	}
	st := Status{State: p.state, Inflight: p.inflight}
	if p.state == Starting || p.state == Ready {
		since, used := p.startedAt, p.lastUsed
		st.Port, st.Since, st.LastUsed = p.port, &since, &used
	}
	if p.err != nil {
		st.Error = p.err.Error()
	}
	return st
}

// UsedMemGB is the estimated memory of models that are starting or running.
func (s *Supervisor) UsedMemGB() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	used := 0.0
	for _, p := range s.procs {
		if p.state == Starting || p.state == Ready {
			used += p.ref.MemGB()
		}
	}
	return used
}

func (s *Supervisor) Logs(name string, n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.procs[name]; p != nil {
		return p.logs.tail(n)
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func closeOnce(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}
