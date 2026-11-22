package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	loginFailed       = problem(502, "login_process_failed", "The login process could not complete.")
	loginTimeout      = problem(504, "login_timeout", "Login timed out. Start a new login.")
	loginBusy         = problem(409, "login_in_progress", "A login operation is already in progress.")
	loginInvalid      = parameter("invalid_login_id", "login_id", "Start a new login and use its login ID.")
	badCode           = parameter("invalid_authorization_code", "code", "Enter a valid single-line authorization code or callback URL.")
	credentialFailure = problem(500, "credential_update_failed", "Could not update the gateway credentials.")
)

const credentialFile = "antigravity-oauth-token"
const authMethodFile = "cache/acp-auth-state.json"

type loginTransaction struct {
	id         string
	pid        int
	stdin      io.WriteCloser
	url        chan string
	done       chan struct{} // closed after Wait and all cleanup
	stop       context.CancelFunc
	expires    time.Time
	err        error  // published by closing done
	email      string // confirmed by the native login command on stdout
	submitting bool   // authManager.mu
}

type authManager struct {
	g                               *gateway
	mu                              sync.Mutex
	current                         *loginTransaction
	loggingOut                      bool
	reportedEmail                   string // guarded by gateway.mu, scoped to the observed file
	fileAtLogin                     os.FileInfo
	commit                          sync.Mutex // credential-changing sections and worker replacement
	ttl, linkTimeout, submitTimeout time.Duration
}

func newAuthManager(g *gateway) *authManager {
	return &authManager{g: g, ttl: 5 * time.Minute, linkTimeout: 15 * time.Second, submitTimeout: 30 * time.Second}
}

func officialAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && (u.Host == "accounts.google.com" || u.Host == "accounts.google.com:443") && u.User == nil && u.EscapedPath() == "/o/oauth2/auth" && u.Fragment == ""
}

func (a *authManager) credentialPath(name string) string {
	return filepath.Join(a.g.cfg.credentialHome, ".gemini", "antigravity-cli", name)
}

type accountMetadata struct {
	file   os.FileInfo
	Email  string `json:"id_token_email"`
	Method string `json:"auth_method"`
}

func (a *authManager) account() (accountMetadata, error) {
	var account accountMetadata
	if a.g.cfg.credentialHome == "" {
		return account, os.ErrNotExist
	}
	f, err := os.OpenFile(a.credentialPath(credentialFile), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return account, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return account, credentialFailure
	}
	err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&account)
	if err != nil {
		return accountMetadata{}, err
	}
	account.Email = validAccountEmail(account.Email)
	account.file = info
	if account.Email == "" && account.Method == "" {
		return accountMetadata{}, credentialFailure
	}
	return account, nil
}

func (a *authManager) allowed() error {
	if a.g.cfg.credentialMode == "external" {
		return problem(403, "credentials_externally_managed", "Manage these credentials on the host.")
	}
	if a.g.cfg.loginCommand == nil {
		return problem(400, "auth_mode_unsupported", "Login is unavailable in mock mode.")
	}
	return nil
}

func validAccountEmail(email string) string {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return ""
	}
	return email
}

func sameCredentialFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

type loginOutput struct {
	email   string
	failure *failure
}

// Parse only the native CLI's success line on stdout. Raw output, including
// codes and provider error descriptions, stays out of status and logs.
func scanLogin(r *os.File, urls chan<- string, result chan<- loginOutput) {
	defer r.Close()
	var out loginOutput
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case urls != nil && strings.HasPrefix(line, "https://"):
			select {
			case urls <- line:
			default:
			}
		case urls != nil && strings.HasPrefix(line, "Logged in as ") && strings.HasSuffix(line, "."):
			out.email = validAccountEmail(strings.TrimSuffix(strings.TrimPrefix(line, "Logged in as "), "."))
		case strings.Contains(line, `oauth2: "invalid_grant"`):
			out.failure = badCode
		case strings.Contains(line, "failed to save credentials:") || strings.Contains(line, "failed to save token:"):
			out.failure = credentialFailure
		}
	}
	_, _ = io.Copy(io.Discard, r)
	result <- out
}

func (a *authManager) launchLocked() (*loginTransaction, error) {
	cmd := a.g.cfg.loginCommand()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, loginFailed
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, loginFailed
	}
	stderr, errWriter, err := os.Pipe()
	if err != nil {
		reader.Close()
		writer.Close()
		stdin.Close()
		return nil, loginFailed
	}
	cmd.Stdout, cmd.Stderr = writer, errWriter
	err = cmd.Start()
	writer.Close()
	errWriter.Close()
	if err != nil {
		reader.Close()
		stderr.Close()
		stdin.Close()
		return nil, loginFailed
	}
	ctx, stop := context.WithTimeout(a.g.ctx, a.ttl)
	expires, _ := ctx.Deadline()
	t := &loginTransaction{id: uid("login_"), pid: cmd.Process.Pid, stdin: stdin, url: make(chan string, 1), done: make(chan struct{}), stop: stop, expires: expires}
	a.current = t
	stdoutResult, stderrResult := make(chan loginOutput, 1), make(chan loginOutput, 1)
	go scanLogin(reader, t.url, stdoutResult)
	go scanLogin(stderr, nil, stderrResult)
	go func() {
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		select {
		case t.err = <-wait:
		case <-ctx.Done():
			_ = syscall.Kill(-t.pid, syscall.SIGKILL)
			<-wait
			t.err = ctx.Err()
		}
		_ = syscall.Kill(-t.pid, syscall.SIGKILL)
		_ = reader.SetReadDeadline(time.Now().Add(time.Second))
		_ = stderr.SetReadDeadline(time.Now().Add(time.Second))
		out, diag := <-stdoutResult, <-stderrResult
		if t.err == nil {
			t.email = out.email
		} else if ctx.Err() == nil {
			if diag.failure != nil {
				t.err = diag.failure
			} else if out.failure != nil {
				t.err = out.failure
			}
		}
		stop()
		stdin.Close()
		a.mu.Lock()
		if a.current == t && !t.submitting {
			a.current = nil
		}
		close(t.done)
		a.mu.Unlock()
	}()
	return t, nil
}

func (a *authManager) start(ctx context.Context) (obj, error) {
	a.mu.Lock()
	if a.current != nil || a.loggingOut {
		a.mu.Unlock()
		return nil, loginBusy
	}
	t, err := a.launchLocked()
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(a.linkTimeout)
	defer timer.Stop()
	select {
	case raw := <-t.url:
		if officialAuthURL(raw) {
			return obj{"login_id": t.id, "auth_url": raw, "expires_in_s": int(time.Until(t.expires).Seconds() + 0.999)}, nil
		}
		err = loginFailed
	case <-timer.C:
		err = loginTimeout
	case <-t.done:
		if errors.Is(t.err, context.DeadlineExceeded) {
			return nil, loginTimeout
		}
		return nil, loginFailed
	case <-ctx.Done():
		err = ctx.Err()
	}
	t.stop()
	<-t.done
	return nil, err
}

func (a *authManager) complete(id, code string) (obj, error) {
	if strings.ContainsAny(code, "\r\n") || strings.TrimSpace(code) == "" {
		return nil, badCode
	}
	a.mu.Lock()
	t := a.current
	if t == nil || id != t.id || time.Now().After(t.expires) {
		a.mu.Unlock()
		return nil, loginInvalid
	}
	if t.submitting || a.loggingOut {
		a.mu.Unlock()
		return nil, loginBusy
	}
	t.submitting = true
	a.mu.Unlock()

	a.commit.Lock()
	defer a.commit.Unlock()
	defer func() {
		t.stop()
		<-t.done
		a.mu.Lock()
		if a.current == t {
			a.current = nil
		}
		a.mu.Unlock()
	}()
	select {
	case <-t.done:
		return nil, loginInvalid
	default:
	}
	a.g.pauseWorker()
	defer a.g.resumeWorker(workerReconnecting)
	path := a.credentialPath(credentialFile)
	before, statErr := os.Stat(path)
	// Include a blocked stdin write in the submission deadline.
	timer := time.AfterFunc(a.submitTimeout, t.stop)
	defer timer.Stop()
	_, writeErr := io.WriteString(t.stdin, strings.TrimSpace(code)+"\n")
	t.stdin.Close()
	<-t.done
	if !timer.Stop() || errors.Is(t.err, context.DeadlineExceeded) {
		return nil, loginTimeout
	}
	if t.err == badCode || t.err == credentialFailure {
		return nil, t.err
	}
	if writeErr != nil || t.err != nil {
		return nil, loginFailed
	}
	account, err := a.account()
	email := t.email
	if email == "" {
		// Compatibility with a file-backed CLI that exits silently. A stale
		// file from the previous account cannot confirm this transaction.
		if err != nil || (statErr != nil && !errors.Is(statErr, os.ErrNotExist)) || sameCredentialFile(before, account.file) {
			return nil, credentialFailure
		}
		email = account.Email
	}
	a.g.mu.Lock()
	a.reportedEmail, a.fileAtLogin = t.email, account.file
	a.g.hadLogin = true
	a.g.mu.Unlock()
	return obj{"ok": true, "email": email}, nil
}

func (a *authManager) cancel(id string) error {
	a.mu.Lock()
	t := a.current
	if t != nil && id != "" && t.id != id {
		a.mu.Unlock()
		return loginInvalid
	}
	if t != nil && t.submitting {
		a.mu.Unlock()
		return loginBusy
	}
	if t != nil {
		t.stop()
	}
	a.mu.Unlock()
	if t != nil {
		<-t.done
	}
	return nil
}

func (a *authManager) shutdown() {
	a.mu.Lock()
	t := a.current
	if t != nil {
		t.stop()
	}
	a.mu.Unlock()
	if t != nil {
		<-t.done
	}
}

func (a *authManager) logout() error {
	a.commit.Lock()
	defer a.commit.Unlock()
	a.mu.Lock()
	a.loggingOut = true
	t := a.current
	if t != nil {
		t.stop()
	}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.loggingOut = false; a.mu.Unlock() }()
	a.g.pauseWorker()
	if t != nil {
		<-t.done
	}
	// A fresh initialized router can log out either native storage backend,
	// even while the supervisor is waiting for credentials or recovering.
	var logoutErr error
	if native := a.g.cfg.nativeACP; native != nil {
		ctx, cancel := context.WithTimeout(a.g.ctx, 5*time.Second)
		logoutErr = a.nativeLogout(ctx, *native)
		cancel()
	}
	for _, name := range []string{credentialFile, authMethodFile, "acp_auth_state.json"} {
		if err := os.Remove(a.credentialPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			logoutErr = err
		}
	}
	a.g.mu.Lock()
	a.reportedEmail, a.fileAtLogin = "", nil
	a.g.mu.Unlock()
	if logoutErr != nil {
		a.g.resumeWorker(workerReconnecting)
		return credentialFailure
	}
	a.g.resumeWorker(loggedOut)
	return nil
}

func (a *authManager) nativeLogout(ctx context.Context, native command) error {
	proc, err := a.g.startACP(ctx, native)
	if err != nil {
		return err
	}
	defer proc.close()
	if err = proc.initialize(ctx); err != nil {
		return err
	}
	_, err = proc.call(ctx, "logout", obj{})
	return err
}
