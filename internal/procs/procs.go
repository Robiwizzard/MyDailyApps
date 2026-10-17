// Package procs starts apps as external processes, checks they are ready,
// stops them (whole process tree) and keeps a state file so a crashed
// session can be cleaned up and a second host instance is refused.
package procs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/shell"
)

// ReadyTimeout is how long an app may take before its port answers.
var ReadyTimeout = 60 * time.Second

// StopGrace is how long a polite stop waits before killing the tree.
var StopGrace = 5 * time.Second

// Spec describes how to start one app.
type Spec struct {
	ID      string
	Dir     string
	Command string
	Port    int    // 0 = pick a free port
	Path    string // URL path to open
	Env     []string
}

// Proc is one running app.
type Proc struct {
	ID        string
	PID       int
	Port      int
	URL       string
	Command   string
	StartedAt time.Time

	cmd     *exec.Cmd
	done    chan struct{}
	ready   chan struct{}
	exitErr error
	logs    *Ring
	plat    platformProc
	stopped bool // stop was requested (exit is expected)
}

// Ready reports whether the port answered.
func (p *Proc) Ready() bool {
	select {
	case <-p.ready:
		return true
	default:
		return false
	}
}

// Manager owns all running apps.
type Manager struct {
	mu       sync.Mutex
	procs    map[string]*Proc
	logs     map[string]*Ring // kept after exit for the log view
	state    *StateFile
	onChange func()
	platform platformManager
	starting map[string]chan struct{}
}

func NewManager(state *StateFile, onChange func()) (*Manager, error) {
	pm, err := newPlatformManager()
	if err != nil {
		return nil, err
	}
	if onChange == nil {
		onChange = func() {}
	}
	return &Manager{procs: map[string]*Proc{}, logs: map[string]*Ring{}, state: state, onChange: onChange, platform: pm, starting: map[string]chan struct{}{}}, nil
}

// Status of one app: "running", "starting" or "" (stopped).
func (m *Manager) Status(id string) (status string, url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[id]; ok {
		if p.Ready() {
			return "running", p.URL
		}
		return "starting", p.URL
	}
	if _, ok := m.starting[id]; ok {
		return "starting", ""
	}
	return "", ""
}

// Logs returns the captured output lines of an app (running or last run).
func (m *Manager) Logs(id string) []string {
	m.mu.Lock()
	r := m.logs[id]
	m.mu.Unlock()
	if r == nil {
		return []string{}
	}
	return r.Lines()
}

// Start launches an app and waits until its port accepts connections.
func (m *Manager) Start(ctx context.Context, s Spec) (string, error) {
	m.mu.Lock()
	if p, ok := m.procs[s.ID]; ok {
		m.mu.Unlock()
		return waitReady(ctx, p)
	}
	if _, ok := m.starting[s.ID]; ok {
		m.mu.Unlock()
		return "", errors.New("the app is already starting")
	}
	m.starting[s.ID] = make(chan struct{})
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.starting, s.ID)
		m.mu.Unlock()
		m.onChange()
	}()
	m.onChange()

	port := s.Port
	if port == 0 {
		var err error
		if port, err = freePort(); err != nil {
			return "", err
		}
	} else if portInUse(port) {
		return "", fmt.Errorf("port %d is already in use by another program", port)
	}
	path := s.Path
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	line := shell.ResolveLaunch(s.Dir, s.Command)
	cmd := shell.Command(context.Background(), s.Dir, line)
	cmd.Env = append(append(os.Environ(), s.Env...), "PORT="+strconv.Itoa(port))
	logs := NewRing(2000)
	logs.Add("$ " + s.Command)
	cmd.Stdout, cmd.Stderr = logs, logs
	m.platform.prepare(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("could not start %q: %w", s.Command, err)
	}
	p := &Proc{
		ID: s.ID, PID: cmd.Process.Pid, Port: port, Command: s.Command, StartedAt: time.Now(),
		URL: fmt.Sprintf("http://localhost:%d%s", port, path),
		cmd: cmd, done: make(chan struct{}), ready: make(chan struct{}), logs: logs,
	}
	p.plat = m.platform.attach(cmd)

	m.mu.Lock()
	m.procs[s.ID] = p
	m.logs[s.ID] = logs
	m.mu.Unlock()
	m.saveState()

	go func() {
		err := cmd.Wait()
		p.exitErr = err
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		logs.Add(fmt.Sprintf("[process exited with code %d]", code))
		// Children may still hold the port: make sure the tree is gone.
		m.platform.kill(p)
		m.platform.release(p)
		close(p.done)
		m.mu.Lock()
		if m.procs[s.ID] == p {
			delete(m.procs, s.ID)
		}
		m.mu.Unlock()
		m.saveState()
		m.onChange()
	}()
	go func() {
		deadline := time.Now().Add(ReadyTimeout)
		for time.Now().Before(deadline) {
			select {
			case <-p.done:
				return
			default:
			}
			if portAnswers(port) {
				close(p.ready)
				m.onChange()
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	url, err := waitReady(ctx, p)
	if err != nil && !errors.Is(err, context.Canceled) {
		// Leave nothing half-started.
		m.Stop(s.ID, false)
	}
	return url, err
}

func waitReady(ctx context.Context, p *Proc) (string, error) {
	timer := time.NewTimer(ReadyTimeout + time.Second)
	defer timer.Stop()
	select {
	case <-p.ready:
		return p.URL, nil
	case <-p.done:
		tail := p.logs.Tail(8)
		return "", fmt.Errorf("the app exited before it was ready. Last output:\n%s", strings.Join(tail, "\n"))
	case <-timer.C:
		return "", fmt.Errorf("the app did not answer on port %d within %s", p.Port, ReadyTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Stop asks the app to exit and kills its whole process tree if it has not
// exited after StopGrace. force kills immediately.
func (m *Manager) Stop(id string, force bool) error {
	m.mu.Lock()
	p, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	p.stopped = true
	if !force {
		if err := m.platform.polite(p); err == nil {
			select {
			case <-p.done:
				return nil
			case <-time.After(StopGrace):
			}
		}
	}
	m.platform.kill(p)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// StopAll stops every app in parallel.
func (m *Manager) StopAll(force bool) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) { defer wg.Done(); m.Stop(id, force) }(id)
	}
	wg.Wait()
}

// Running returns the ids of running apps.
func (m *Manager) Running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) saveState() {
	if m.state == nil {
		return
	}
	m.mu.Lock()
	var apps []StateApp
	for _, p := range m.procs {
		apps = append(apps, StateApp{ID: p.ID, PID: p.PID, Started: p.StartedAt.UnixNano()})
	}
	m.mu.Unlock()
	_ = m.state.SetApps(apps)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func portAnswers(port int) bool {
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		c, err := net.DialTimeout("tcp", host+":"+strconv.Itoa(port), 300*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}

func portInUse(port int) bool { return portAnswers(port) }

// Ring keeps the last N output lines.
type Ring struct {
	mu    sync.Mutex
	lines []string
	max   int
	part  []byte
}

func NewRing(max int) *Ring { return &Ring{max: max} }

func (r *Ring) Add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(line)
}

func (r *Ring) add(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

func (r *Ring) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.part = append(r.part, b...)
	for {
		i := -1
		for j, c := range r.part {
			if c == '\n' {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		r.add(strings.TrimRight(string(r.part[:i]), "\r"))
		r.part = r.part[i+1:]
	}
	if len(r.part) > 8192 { // very long line without newline
		r.add(string(r.part))
		r.part = nil
	}
	return len(b), nil
}

func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.lines...)
	if len(r.part) > 0 {
		out = append(out, string(r.part))
	}
	return out
}

func (r *Ring) Tail(n int) []string {
	l := r.Lines()
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return l
}
