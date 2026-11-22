package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type command struct {
	observeStatus bool
	observerMode  string
	bin           string
	args, env     []string
	dir           string
}

// Capture the native executable and its environment before an optional ACP adapter wraps it.
func (c command) login() *exec.Cmd {
	cmd := exec.Command(c.bin, "acp", "--login")
	cmd.Env, cmd.Dir = c.env, c.dir
	return cmd
}

func endpointOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" || u.Path != "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// Real-binary acceptance tests call this before starting their local mock listeners.
func offlineNamespace() error {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return err
	}
	seen := false
	for _, line := range strings.Split(string(b), "\n") {
		name, _, ok := strings.Cut(line, ":")
		if ok {
			if strings.TrimSpace(name) != "lo" {
				return errors.New("real ACP offline mode requires a loopback-only network namespace")
			}
			seen = true
		}
	}
	if !seen {
		return errors.New("cannot verify offline network namespace")
	}
	return nil
}

const cleanAgentName = "gateway-clean"

const cleanAgentMarkdown = `---
name: gateway-clean
description: Minimal gateway agent without built-in coding tools or default system prompt
mainAgent: true
excludeDefaultComponents: true
inheritCustomizations: false
inheritMcp: true
mcpServers:
  - name: vbridge
    url: http://127.0.0.1:1/unused
---
`

func writeCleanAgent(cwd string) error {
	dir := filepath.Join(cwd, ".agents", "agents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cleanAgentName+".md"), []byte(cleanAgentMarkdown), 0600)
}

func nativeCommand(bin, agent, cwd string) (command, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return command{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return command{}, err
	}
	args := []string{"acp", "--stdio", "--process-per-session", "--disable-telemetry", "--include-default-customizations=false"}
	switch agent {
	case "":
		if err = writeCleanAgent(cwd); err != nil {
			return command{}, err
		}
		args = append(args, "--agent", cleanAgentName)
	case "default":
		// Explicitly opt into agy's built-in coding agent without provisioning gateway-clean.
	default:
		args = append(args, "--agent", agent)
	}
	return command{bin: path, args: args, dir: cwd, env: withLoopbackNoProxy(os.Environ())}, nil
}
func offlineCommand(bin, endpoint, agent, home, cwd string) (command, error) {
	return offlineCredentialCommand(bin, endpoint, agent, home, cwd, true)
}
func offlineCredentialCommand(bin, endpoint, agent, home, cwd string, seed bool) (command, error) {
	if !endpointOK(endpoint) {
		return command{}, errors.New("mock CCPA endpoint must be a literal loopback HTTP address with a port")
	}
	cmd, err := nativeCommand(bin, agent, cwd)
	if err != nil {
		return command{}, err
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return command{}, err
	}
	if seed {
		fake := obj{"auth_method": "consumer", "id_token_email": "test@example.invalid", "token": obj{"access_token": "offline-test-token", "token_type": "Bearer", "refresh_token": "offline-test-refresh", "expires_in": 3600, "expiry": "2099-01-01T00:00:00Z"}}
		if err = os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte(enc(fake)), 0600); err != nil {
			return command{}, err
		}
	}
	cmd.args = append(cmd.args, "--cloud_code_endpoint", endpoint, "--log-file", filepath.Join(home, "acp.log"))
	cmd.env = []string{"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/bin:/bin", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,::1"}
	return cmd, nil
}
func (g *gateway) unavailableLocked() *failure {
	if g.authError != nil {
		return g.authError
	}
	return workerUnavailable
}

// A retired generation must not put its replacement back into auth wait.
func (g *gateway) requireAuth(p *peer, f *failure) {
	if f.code != "acp_auth_required" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.worker == p {
		g.authError = f
		g.retryingAuth = true
		g.catalog = nil
		p.stop()
	}
}

type acpProcess struct {
	*peer
	version string
	stdout  *os.File
	exited  <-chan struct{}
	close   func()
}

// Both the supervisor and explicit logout use the same native ACP lifecycle.
func (g *gateway) startACP(ctx context.Context, c command) (*acpProcess, error) {
	cmd := exec.Command(c.bin, c.args...)
	cmd.Dir = c.dir
	cmd.Env = c.env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own the read end so Cmd.Wait cannot close stdout before the reader drains it.
	reader, writer, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stdout = writer
	cmd.Stderr = io.Discard
	err = cmd.Start()
	writer.Close()
	if err != nil {
		reader.Close()
		stdin.Close()
		return nil, err
	}
	p := newPeer(stdin, g.incoming)
	p.observeStatus = c.observeStatus
	go p.read(reader)
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	kill := func(sig syscall.Signal) { _ = syscall.Kill(-cmd.Process.Pid, sig) }
	lifetime, stop := context.WithCancel(ctx)
	closeProcess := func() {
		stop()
		p.stop()
		kill(syscall.SIGTERM)
		select {
		case <-waited:
		case <-time.After(time.Second):
			kill(syscall.SIGKILL)
			<-waited
		}
		kill(syscall.SIGKILL)
		reader.Close()
	}
	// Also unblock a full stdin pipe if the worker stalls during shutdown.
	go func() {
		select {
		case <-lifetime.Done():
			stdin.Close()
		case <-waited:
			stdin.Close()
		}
	}()
	return &acpProcess{peer: p, stdout: reader, exited: waited, close: closeProcess}, nil
}

func (p *acpProcess) initialize(ctx context.Context) error {
	v, err := p.call(ctx, "initialize", obj{"protocolVersion": 1, "clientCapabilities": obj{}, "clientInfo": obj{"name": "agy-acp-gateway", "version": "1"}})
	if err != nil {
		return err
	}
	if fmt.Sprint(v["protocolVersion"]) != "1" {
		return errors.New("unsupported ACP protocol version")
	}
	p.version = str(asObj(v["agentInfo"])["version"])
	return nil
}

func runProcess(ctx context.Context, c command, g *gateway) (time.Duration, error) {
	proc, err := g.startACP(ctx, c)
	if err != nil {
		return 0, err
	}
	defer proc.close()
	p := proc.peer
	boot, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := proc.initialize(boot); err != nil {
		return 0, err
	}
	info, err := g.discoverModels(boot, p)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	g.mu.Lock()
	if ctx.Err() != nil || g.paused {
		g.mu.Unlock()
		return 0, workerReconnecting
	}
	g.worker = p
	g.authError = nil
	g.hadLogin, g.retryingAuth = true, false
	g.catalog = nil
	g.modelDetails = map[string]modelInfo{}
	g.version, g.observerMode = proc.version, c.observerMode
	if g.observerMode == "" {
		g.observerMode = "acp-only"
	}
	g.lastObservation = ""
	models := asObj(info["models"])
	g.defaultModel = str(models["currentModelId"])
	for _, x := range arr(models["availableModels"]) {
		m := asObj(x)
		id := str(m["modelId"])
		if _, exists := g.modelDetails[id]; id != "" && !exists {
			g.catalog = append(g.catalog, id)
			g.modelDetails[id] = modelInfo{Name: str(m["name"])}
		}
	}
	g.updateThoughtLevelsLocked(g.defaultModel, arr(info["configOptions"]))
	tier := asObj(asObj(info["_meta"])["antigravity"])["tier"]
	g.tier = str(tier)
	for _, field := range []string{"name", "displayName", "id"} {
		if g.tier == "" {
			g.tier = str(asObj(tier)[field])
		}
	}
	g.lastModelUse = start
	g.mu.Unlock()
	if g.authKeepaliveEnabled() {
		life, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); g.maintainLogin(life, p) }()
		defer func() { stop(); <-done }()
	}
	select {
	case <-ctx.Done():
	case <-proc.exited:
		// Drain replies already written before exit, including the final RPC error.
		_ = proc.stdout.SetReadDeadline(time.Now().Add(time.Second))
		select {
		case <-p.done:
		case <-ctx.Done():
		}
	case <-p.done:
	}
	g.mu.Lock()
	p.stop()
	if g.worker == p {
		g.worker = nil
		g.catalog = nil
	}
	for _, s := range g.sessions {
		if s.peer == p {
			// A busy handler owns the pending RPC. Let it receive the buffered
			// reply or the reader's terminal error before releasing its turn.
			g.forgetLocked(s)
			if !s.busy && s.turn != nil {
				s.turn.cancel()
			}
		}
	}
	g.mu.Unlock()
	return time.Since(start), workerLost
}

// Opening a native session runs agy's silent credential restore, including its
// refresh-and-save path when due. No generation or interactive login is used.
func (g *gateway) discoverModels(boot context.Context, p *peer) (obj, error) {
	info, err := p.call(boot, "session/new", obj{"cwd": g.cfg.cwd, "mcpServers": []any{}, "_meta": obj{"disableBuiltInTools": true}})
	if err != nil {
		return nil, err
	}
	id := str(info["sessionId"])
	if id == "" {
		return nil, errors.New("model discovery returned no session ID")
	}
	closeSession(p, id)
	for _, x := range arr(asObj(info["models"])["availableModels"]) {
		if str(asObj(x)["modelId"]) != "" {
			return info, nil
		}
	}
	return nil, errors.New("empty ACP model catalog")
}

func (g *gateway) authKeepaliveEnabled() bool {
	return g.cfg.nativeACP != nil && g.cfg.credentialMode == "managed" && g.cfg.authKeepalive > 0
}

func (g *gateway) maintainLogin(ctx context.Context, owner *peer) {
	interval := g.cfg.authKeepalive
	retry := min(interval, time.Minute)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		g.mu.Lock()
		if g.worker != owner || g.paused {
			g.mu.Unlock()
			return
		}
		remaining := interval - time.Since(g.lastModelUse)
		if g.activeLocked() > 0 {
			remaining = interval
		}
		g.mu.Unlock()
		if remaining > 0 {
			timer.Reset(remaining)
			continue
		}

		// An isolated, bounded native router can be reaped even if session/new
		// times out before returning its ID. User sessions remain on owner.
		probe, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := g.checkSavedLogin(probe)
		cancel()
		g.mu.Lock()
		if ctx.Err() != nil || g.worker != owner {
			g.mu.Unlock()
			return
		}
		g.lastAuthCheck = time.Now()
		g.authCheckError = ""
		wait := interval
		if err != nil {
			g.authCheckError = publicFailure(err).code
			wait = retry
			retry += min(retry, interval-retry)
		} else {
			retry = min(interval, time.Minute)
		}
		g.mu.Unlock()
		timer.Reset(wait)
	}
}

func (g *gateway) checkSavedLogin(ctx context.Context) error {
	proc, err := g.startACP(ctx, *g.cfg.nativeACP)
	if err != nil {
		return err
	}
	defer proc.close()
	if err = proc.initialize(ctx); err != nil {
		return err
	}
	_, err = g.discoverModels(ctx, proc.peer)
	return err
}

func supervise(ctx context.Context, c command, g *gateway) {
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		g.mu.Lock()
		if g.paused {
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-g.wake:
				continue
			}
		}
		select {
		case <-g.wake:
		default:
		}
		run, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		g.runCancel, g.runDone = stop, done
		g.mu.Unlock()
		up, err := runProcess(run, c, g)
		f := publicFailure(err)
		var saved bool
		if f.code == "acp_auth_required" {
			_, err := g.auth.account()
			saved = err == nil
		}
		g.mu.Lock()
		retired := run.Err() != nil
		stop()
		g.runCancel, g.runDone = nil, nil
		close(done)
		if retired {
			g.mu.Unlock()
			continue
		}
		if ctx.Err() != nil {
			g.mu.Unlock()
			return
		}
		if up == 0 {
			g.recordProbeFailureLocked(f, saved)
		}
		retrying := g.retryingAuth
		waiting := g.needsAuthLocked()
		g.mu.Unlock()
		wait := delay
		if retrying {
			wait = 100 * time.Millisecond
		} else if waiting {
			wait = 30 * time.Second
			slog.Info("ACP worker waiting for authentication")
		} else {
			slog.Warn("ACP worker stopped", "code", publicFailure(err).code, "restart_ms", wait.Milliseconds())
		}
		select {
		case <-ctx.Done():
			return
		case <-g.wake:
		case <-time.After(wait):
		}
		if up > time.Minute {
			delay = 100 * time.Millisecond
		} else {
			delay = min(4*time.Second, 2*delay)
		}
	}
}

func withLoopbackNoProxy(env []string) []string {
	result := make([]string, 0, len(env)+2)
	var rules []string
	seen := make(map[string]bool)
	add := func(raw string) {
		for _, item := range strings.Split(raw, ",") {
			rule := strings.TrimSpace(item)
			key := strings.ToLower(rule)
			if key != "" && !seen[key] {
				seen[key] = true
				rules = append(rules, rule)
			}
		}
	}
	for _, item := range env {
		key, value, _ := strings.Cut(item, "=")
		if key == "NO_PROXY" || key == "no_proxy" {
			add(value)
		} else {
			result = append(result, item)
		}
	}
	add("127.0.0.1,::1,localhost")
	rule := strings.Join(rules, ",")
	return append(result, "NO_PROXY="+rule, "no_proxy="+rule)
}

var workerReconnecting = problem(503, "worker_reconnecting", "The gateway is reconnecting after an account change.")
var loggedOut = problem(503, "acp_auth_required", "Authenticate the ACP worker before retrying.")

// A fresh CLI process can restore or refresh its saved session. After a prior
// login, give it one independent confirmation before inviting another login.
func (g *gateway) recordProbeFailureLocked(f *failure, saved bool) {
	g.retryingAuth = f.code == "acp_auth_required" && !g.needsAuthLocked() && (g.hadLogin || saved)
	if f.code == "acp_auth_required" {
		g.authError = f
		if !g.retryingAuth {
			// Remember that a login existed, but do not keep attributing a
			// later externally restored keyring session to an old identity.
			g.auth.reportedEmail, g.auth.fileAtLogin = "", nil
		}
	} else if g.needsAuthLocked() {
		// The latest probe failed for another reason, e.g. a network outage.
		g.authError = nil
	}
}

func (g *gateway) needsAuthLocked() bool {
	return g.authError != nil && g.authError.code == "acp_auth_required"
}

// Revoke admission and all client capabilities before allowing the CLI to change credentials.
// Waiting for runDone also covers a discovery process that has not published its peer yet.
func (g *gateway) pauseWorker() {
	g.mu.Lock()
	g.paused = true
	g.retryingAuth = false
	g.lastAuthCheck = time.Time{}
	g.authCheckError = ""
	g.authError = workerReconnecting
	p, stop, done := g.worker, g.runCancel, g.runDone
	g.worker, g.catalog, g.defaultModel = nil, nil, ""
	for _, s := range g.sessions {
		g.forgetLocked(s)
		if s.turn != nil {
			s.turn.abort(workerReconnecting)
			s.turn.cancel()
		}
	}
	g.mu.Unlock()
	if stop != nil {
		stop()
	}
	if p != nil {
		p.stop()
	}
	if done != nil {
		<-done
	}
}

func (g *gateway) resumeWorker(cause *failure) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused, g.authError = false, cause
	if cause == loggedOut {
		g.hadLogin = false
	}
	select {
	case g.wake <- struct{}{}:
	default:
	}
}
