package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoopbackProxyRules(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{nil, "127.0.0.1,::1,localhost"},
		{[]string{"NO_PROXY=10.0.0.0/8,localhost", "no_proxy=localhost,.corp", "HTTPS_PROXY=http://proxy"}, "10.0.0.0/8,localhost,.corp,127.0.0.1,::1"},
		{[]string{"no_proxy=::1,LOCALHOST,127.0.0.1"}, "::1,LOCALHOST,127.0.0.1"},
	} {
		got := withLoopbackNoProxy(tc.env)
		if !slices.Contains(got, "NO_PROXY="+tc.want) || !slices.Contains(got, "no_proxy="+tc.want) || !slices.Equal(got, withLoopbackNoProxy(got)) {
			t.Fatal(got)
		}
		if slices.Contains(tc.env, "HTTPS_PROXY=http://proxy") && !slices.Contains(got, "HTTPS_PROXY=http://proxy") {
			t.Fatal(got)
		}
	}
}

func TestNativeWorkerAndLoginProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://synthetic-proxy.invalid:8080")
	t.Setenv("NO_PROXY", "example.invalid")
	t.Setenv("no_proxy", "10.0.0.0/8")
	c, err := nativeCommand(os.Args[0], "default", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	login := c.login()
	for _, env := range [][]string{c.env, login.Env} {
		for _, rule := range []string{"HTTPS_PROXY=http://synthetic-proxy.invalid:8080", "NO_PROXY=", "no_proxy="} {
			if strings.HasSuffix(rule, "=") {
				var rules []string
				for _, line := range env {
					if strings.HasPrefix(line, rule) {
						rules = strings.Split(strings.TrimPrefix(line, rule), ",")
					}
				}
				for _, v := range []string{"example.invalid", "10.0.0.0/8", "127.0.0.1", "localhost", "::1"} {
					if !slices.Contains(rules, v) {
						t.Fatal(rule, v)
					}
				}
			} else if !slices.Contains(env, rule) {
				t.Fatal(rule)
			}
		}
	}
}

// A separate executable owns stdin/stdout and writes only synthetic credentials.
func TestLoginChild(t *testing.T) {
	mode := os.Getenv("GATEWAY_LOGIN_FIXTURE")
	if mode == "" {
		return
	}
	home := os.Getenv("HOME")
	link := "https://accounts.google.com/o/oauth2/auth?state=private-test-state"
	if mode == "badurl" {
		link = "https://accounts.google.com.evil.invalid/o/oauth2/auth"
	}
	if mode != "silent" {
		fmt.Println(link)
	}
	if mode == "silent" {
		time.Sleep(time.Minute)
		os.Exit(1)
	}
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		os.Exit(1)
	}
	if mode == "stall" {
		time.Sleep(time.Minute)
	}
	if mode == "invalid-grant" {
		fmt.Fprintln(os.Stderr, `login failed: oauth2: "invalid_grant" "private-test-code"`)
		os.Exit(2)
	}
	if mode == "save-failed" {
		fmt.Fprintln(os.Stderr, "failed to save credentials: private-test-code")
		os.Exit(2)
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "private-test-code")
		os.Exit(2)
	}
	if mode == "gated" {
		_ = os.WriteFile(filepath.Join(home, "submitted"), nil, 0600)
		for {
			if _, e := os.Stat(filepath.Join(home, "release")); e == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	switch mode {
	case "native-success":
		fmt.Println("Logged in as keyring@example.invalid.")
		os.Exit(0)
	case "stderr-success":
		fmt.Fprintln(os.Stderr, "Logged in as wrong@example.invalid.")
		os.Exit(0)
	case "false-success":
		fmt.Println("Logged in as wrong@example.invalid.")
		os.Exit(2)
	}
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if os.MkdirAll(dir, 0700) != nil {
		os.Exit(2)
	}
	email := os.Getenv("GATEWAY_FIXTURE_EMAIL")
	if email == "" {
		email = "b@example.invalid"
	}
	b := obj{"auth_method": "consumer", "id_token_email": email, "token": obj{"access_token": "private-test-access", "token_type": "Bearer", "refresh_token": "private-test-refresh", "expiry": "2099-01-01T00:00:00Z"}}
	if mode != "no-file" && os.WriteFile(filepath.Join(dir, credentialFile), []byte(enc(b)), 0600) != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func testLogin(home, mode string) func() *exec.Cmd {
	return func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLoginChild$")
		cmd.Env = []string{"HOME=" + home, "GATEWAY_LOGIN_FIXTURE=" + mode, "GORACE=atexit_sleep_ms=0"}
		return cmd
	}
}
func authConfig(t *testing.T, mode string) config {
	t.Helper()
	c := defaults()
	c.mode = "live"
	c.key = "fixture"
	c.credentialHome = t.TempDir()
	c.loginCommand = testLogin(c.credentialHome, mode)
	return c
}
func adminCall(t *testing.T, g *gateway, path string, body obj, status int, code string) obj {
	t.Helper()
	method := "POST"
	if path == "status" {
		method = "GET"
	}
	w := invoke(context.Background(), g, method, "/admin/api/"+path, enc(body), http.Header{"Authorization": {"Bearer fixture"}, "Content-Type": {"application/json"}})
	var v obj
	if json.Unmarshal(w.Body.Bytes(), &v) != nil || w.Code != status || str(asObj(v["error"])["code"]) != code {
		t.Fatalf("%s: got HTTP %d / %s; expected %d / %s", path, w.Code, str(asObj(v["error"])["code"]), status, code)
	}
	for _, marker := range []string{"private-test-refresh", "private-test-access", "private-test-code"} {
		if strings.Contains(w.Body.String(), marker) {
			t.Fatal("exposed private field")
		}
	}
	return v
}
func TestDashboardAccess(t *testing.T) {
	c := authConfig(t, "ok")
	g := fixtureFor(t, c).g
	call := func(method, path, body, ct, key, origin string) *httptest.ResponseRecorder {
		return invoke(context.Background(), g, method, path, body, http.Header{"Authorization": {"Bearer " + key}, "Content-Type": {ct}, "Origin": {origin}})
	}
	page := call("GET", "/", "", "", "", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "AGY ACP Gateway") {
		t.Fatal(page.Code)
	}
	for _, h := range []string{"X-Frame-Options", "X-Content-Type-Options", "Cache-Control", "Referrer-Policy", "Content-Security-Policy"} {
		if page.Header().Get(h) == "" {
			t.Fatal(h)
		}
	}
	for _, path := range []string{"status", "auth/start", "auth/cancel", "auth/complete", "auth/logout"} {
		if w := call("POST", "/admin/api/"+path, "{}", "application/json", "wrong", ""); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	for _, tc := range []struct {
		path, method, ct, body, origin string
		want                           int
	}{
		{"auth/start", "POST", "text/plain", "{}", "", 415}, {"auth/start", "POST", "application/json", "{", "", 400}, {"auth/start", "POST", "application/json", strings.Repeat(" ", 65536) + "{}", "", 413}, {"auth/start", "GET", "", "", "", 405}, {"status", "GET", "", "", "https://elsewhere.invalid", 403},
	} {
		if w := call(tc.method, "/admin/api/"+tc.path, tc.body, tc.ct, "fixture", tc.origin); w.Code != tc.want {
			t.Fatal(tc.path, w.Code)
		}
	}
	c.dashboard = false
	off := newGateway(context.Background(), c)
	for _, path := range []string{"/", "/admin/api/status", "/admin/api/auth/start"} {
		if w := invoke(context.Background(), off, "GET", path, "", nil); w.Code != 404 {
			t.Fatal(path, w.Code)
		}
	}
	c = defaults()
	c.key = "fixture"
	mock := newGateway(context.Background(), c)
	for _, path := range []string{"auth/start", "auth/cancel", "auth/complete", "auth/logout"} {
		adminCall(t, mock, path, obj{}, 400, "auth_mode_unsupported")
	}
}
func TestLoginLifecycle(t *testing.T) {
	g := fixtureFor(t, authConfig(t, "ok")).g
	a := g.auth
	first := adminCall(t, g, "auth/start", obj{}, 200, "")
	id := str(first["login_id"])
	if !officialAuthURL(str(first["auth_url"])) || id == "" {
		t.Fatal("missing transaction")
	}
	a.mu.Lock()
	pid := a.current.pid
	a.mu.Unlock()
	adminCall(t, g, "auth/start", obj{}, 409, "login_in_progress")
	for _, input := range []string{"", " \t", "x\ny", "x\r", "\nx"} {
		adminCall(t, g, "auth/complete", obj{"login_id": id, "code": input}, 400, "invalid_authorization_code")
	}
	adminCall(t, g, "auth/complete", obj{"login_id": "wrong", "code": "hello"}, 400, "invalid_login_id")
	adminCall(t, g, "auth/cancel", obj{"login_id": "wrong"}, 400, "invalid_login_id")
	adminCall(t, g, "auth/cancel", obj{"login_id": id}, 200, "")
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("login process survived cancel")
	}
	adminCall(t, g, "auth/cancel", obj{}, 200, "")
	s := adminCall(t, g, "status", nil, 200, "")
	if asObj(s["acp"])["alive"] != true {
		t.Fatal("cancel disconnected existing worker")
	}
	next := adminCall(t, g, "auth/start", obj{}, 200, "")
	adminCall(t, g, "auth/complete", obj{"login_id": id, "code": "old"}, 400, "invalid_login_id")
	v := adminCall(t, g, "auth/complete", obj{"login_id": next["login_id"], "code": "  private-test-code  "}, 200, "")
	if v["email"] != "b@example.invalid" {
		t.Fatal(v)
	}
	s = adminCall(t, g, "status", nil, 200, "")
	if asObj(s["acp"])["alive"] != false || asObj(s["auth"])["email"] != "b@example.invalid" {
		t.Fatal(s)
	}
	state := a.credentialPath("cache/acp-auth-state.json")
	if err := os.MkdirAll(filepath.Dir(state), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := a.credentialPath("acp_auth_state.json")
	for _, file := range []string{state, legacy} {
		if err := os.WriteFile(file, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	adminCall(t, g, "auth/logout", obj{}, 200, "")
	adminCall(t, g, "auth/logout", obj{}, 200, "")
	for _, name := range []string{state, legacy, a.credentialPath(credentialFile)} {
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if v := adminCall(t, g, "status", nil, 200, ""); asObj(v["auth"])["auth_required"] != true {
		t.Fatal(v)
	}
}
func TestLoginFailures(t *testing.T) {
	for _, mode := range []string{"badurl", "silent", "stall", "fail", "invalid-grant", "save-failed", "no-file", "expiry", "shutdown", "delete", "external"} {
		t.Run(mode, func(t *testing.T) {
			c := authConfig(t, mode)
			if mode == "external" {
				c.credentialMode = "external"
			}
			g := fixtureFor(t, c).g
			a := g.auth
			t.Cleanup(a.shutdown)
			switch mode {
			case "external":
				for _, name := range []string{"start", "complete", "cancel", "logout"} {
					adminCall(t, g, "auth/"+name, obj{}, 403, "credentials_externally_managed")
				}
				if g.worker == nil {
					t.Fatal("external credentials changed")
				}
			case "badurl":
				adminCall(t, g, "auth/start", obj{}, 502, "login_process_failed")
			case "silent":
				a.linkTimeout = 120 * time.Millisecond
				adminCall(t, g, "auth/start", obj{}, 504, "login_timeout")
			case "expiry", "shutdown":
				if mode == "expiry" {
					a.ttl = 650 * time.Millisecond
				}
				v := adminCall(t, g, "auth/start", obj{}, 200, "")
				a.mu.Lock()
				tx := a.current
				a.mu.Unlock()
				if mode == "shutdown" {
					a.shutdown()
				}
				select {
				case <-tx.done:
				case <-time.After(3 * time.Second):
					t.Fatal("orphan login")
				}
				if syscall.Kill(tx.pid, 0) != syscall.ESRCH {
					t.Fatal("login still exists")
				}
				adminCall(t, g, "auth/complete", obj{"login_id": v["login_id"], "code": "late"}, 400, "invalid_login_id")
			case "delete":
				path := a.credentialPath(credentialFile)
				if e := os.MkdirAll(path, 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(filepath.Join(path, "keep"), nil, 0600); e != nil {
					t.Fatal(e)
				}
				adminCall(t, g, "auth/logout", obj{}, 500, "credential_update_failed")
			default:
				a.submitTimeout = 200 * time.Millisecond
				v := adminCall(t, g, "auth/start", obj{}, 200, "")
				n, code := 502, "login_process_failed"
				if mode == "stall" {
					n, code = 504, "login_timeout"
				}
				if mode == "invalid-grant" {
					n, code = 400, "invalid_authorization_code"
				}
				if mode == "no-file" || mode == "save-failed" {
					n, code = 500, "credential_update_failed"
				}
				adminCall(t, g, "auth/complete", obj{"login_id": v["login_id"], "code": "private-test-code"}, n, code)
			}
		})
	}
	for _, raw := range []string{"http://accounts.google.com/o/oauth2/auth", "https://a@accounts.google.com/o/oauth2/auth", "https://accounts.google.com:444/o/oauth2/auth", "https://accounts.google.com.evil.invalid/o/oauth2/auth", "https://accounts.google.com/other", "https://accounts.google.com/o/oauth2/%61uth"} {
		if officialAuthURL(raw) {
			t.Fatal(raw)
		}
	}
}

func TestAccountSwitchRevokesOldCapabilities(t *testing.T) {
	c := authConfig(t, "gated")
	f := fixtureFor(t, c)
	g := f.g
	result := invoke(context.Background(), g, "POST", "/v1/responses", enc(obj{"input": "tools: x", "tools": specs("x")}), http.Header{"Authorization": {"Bearer fixture"}})
	var old obj
	if json.Unmarshal(result.Body.Bytes(), &old) != nil || old["id"] == nil {
		t.Fatal(result.Code, result.Body)
	}
	g.mu.Lock()
	oldSession := g.tips[str(old["id"])]
	oldTurn := oldSession.turn
	oldWorker := g.worker
	g.mu.Unlock()
	v := adminCall(t, g, "auth/start", obj{}, 200, "")
	complete := make(chan error, 1)
	go func() { _, err := g.auth.complete(str(v["login_id"]), "synthetic"); complete <- err }()
	until(t, func() bool { _, e := os.Stat(filepath.Join(c.credentialHome, "submitted")); return e == nil })
	adminCall(t, g, "auth/complete", obj{"login_id": v["login_id"], "code": "again"}, 409, "login_in_progress")
	adminCall(t, g, "auth/start", obj{}, 409, "login_in_progress")
	for _, p := range []string{"models", "responses", "chat/completions"} {
		method := "POST"
		body := enc(chat(user("x")))
		if p == "models" {
			method = "GET"
		}
		if p == "responses" {
			body = enc(obj{"input": "x", "previous_response_id": old["id"]})
		}
		w := invoke(context.Background(), g, method, "/v1/"+p, body, http.Header{"Authorization": {"Bearer fixture"}})
		if w.Code != 503 || !strings.Contains(w.Body.String(), "worker_reconnecting") {
			t.Fatal(p, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	g.mcp(w, httptest.NewRequest("POST", "/mcp/"+oldSession.token, strings.NewReader("{}")))
	if w.Code != 404 || context.Cause(oldTurn.ctx) != workerReconnecting {
		t.Fatal(w.Code, context.Cause(oldTurn.ctx))
	}
	g.requireAuth(oldWorker, loggedOut)
	if e := os.WriteFile(filepath.Join(c.credentialHome, "release"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e := <-complete; e != nil {
		t.Fatal(e)
	}
	g.mu.Lock()
	if g.worker != nil || len(g.tips) != 0 || g.authError != workerReconnecting {
		g.mu.Unlock()
		t.Fatal("old account revived")
	}
	g.mu.Unlock()
}

type disconnectedWriter struct{ *httptest.ResponseRecorder }

func (w disconnectedWriter) Write([]byte) (int, error) { return 0, context.Canceled }

func TestRequestAndTokenAccounting(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%t/stream=%t", responses, stream), func(t *testing.T) {
				c := defaults()
				c.key = "fixture"
				g := fixtureFor(t, c).g
				path := "/v1/chat/completions"
				q := chat(user("length: x"))
				if responses {
					path = "/v1/responses"
					q = obj{"input": "length: x"}
				}
				q["stream"] = stream
				header := http.Header{"Authorization": {"Bearer fixture"}}
				invoke(context.Background(), g, "GET", "/health", "", nil)
				invoke(context.Background(), g, "GET", "/v1/models", "", header)
				adminCall(t, g, "status", nil, 200, "")
				invoke(context.Background(), g, "OPTIONS", path, "", nil)
				invoke(context.Background(), g, "POST", path, enc(q), nil)
				invoke(context.Background(), g, "POST", path, "{", header)
				if w := invoke(context.Background(), g, "POST", path, enc(q), header); w.Code != 200 {
					t.Fatal(w.Code)
				}
				if v := g.usage.snapshot(); enc(v) != enc(obj{"requests_total": 3, "requests_errors": 2, "input_tokens": 12, "output_tokens": 5, "cached_tokens": 3, "reasoning_tokens": 2}) {
					t.Fatal(v)
				}
				r := httptest.NewRequest("POST", path, strings.NewReader(enc(q)))
				r.Header = header
				g.ServeHTTP(disconnectedWriter{httptest.NewRecorder()}, r)
				if g.usage.failed.Load() != 3 || g.usage.total.Load() != 4 {
					t.Fatal(g.usage.snapshot())
				}
			})
		}
	}
	g := fixtureFor(t, defaults()).g
	postJSON(t, g, "responses", obj{"input": "tools: x", "tools": specs("x"), "store": false}, 200)
	if g.usage.total.Load() != 1 || g.usage.failed.Load() != 0 || g.usage.input.Load() != 0 {
		t.Fatal(g.usage.snapshot())
	}
}
func TestOrphanChatHistory(t *testing.T) {
	f := fixtureFor(t, defaults())
	orphan := obj{"role": "tool", "tool_call_id": "missing", "content": "result"}
	for _, messages := range [][]obj{{user("x"), orphan}, {user("x"), orphan, user("next")}} {
		v := postJSON(t, f.g, "chat/completions", chat(messages...), 400)
		if e := asObj(v["error"]); e["code"] != "invalid_tool_history" || e["param"] != "messages" {
			t.Fatal(e)
		}
	}
	if len(f.framesFor("session/new")) != 0 {
		t.Fatal("invalid input changed ACP state")
	}
}

func TestLoginWithoutFileChange(t *testing.T) {
	for _, mode := range []string{"native-success", "stderr-success", "false-success"} {
		t.Run(mode, func(t *testing.T) {
			c := authConfig(t, mode)
			g := fixtureFor(t, c).g
			// An old credential file may coexist with the native keyring.
			path := g.auth.credentialPath(credentialFile)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			old := []byte(`{"id_token_email":"old@example.invalid","auth_method":"consumer"}`)
			if err := os.WriteFile(path, old, 0600); err != nil {
				t.Fatal(err)
			}
			input := adminCall(t, g, "auth/start", obj{}, 200, "")
			body := obj{"login_id": input["login_id"], "code": "private-test-code"}
			switch mode {
			case "native-success":
				v := adminCall(t, g, "auth/complete", body, 200, "")
				s := asObj(adminCall(t, g, "status", nil, 200, "")["auth"])
				if v["email"] != "keyring@example.invalid" || s["email"] != v["email"] || s["status"] != "recovering" {
					t.Fatal(v, s)
				}
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				input = adminCall(t, g, "auth/start", obj{}, 200, "")
				body["login_id"] = input["login_id"]
				adminCall(t, g, "auth/complete", body, 200, "")
				if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("unexpected credential file", e)
				}
				return
			case "stderr-success":
				adminCall(t, g, "auth/complete", body, 500, "credential_update_failed")
			case "false-success":
				adminCall(t, g, "auth/complete", body, 502, "login_process_failed")
			}
			current, err := os.ReadFile(path)
			if err != nil || string(current) != string(old) {
				t.Fatal("existing credentials changed")
			}
		})
	}
}

func TestSavedLoginRecoveryState(t *testing.T) {
	g := newGateway(context.Background(), authConfig(t, "ok"))
	g.hadLogin = true
	check := func(f *failure, saved bool, want string) {
		t.Helper()
		g.mu.Lock()
		g.recordProbeFailureLocked(f, saved)
		g.mu.Unlock()
		auth := asObj(adminCall(t, g, "status", nil, 200, "")["auth"])
		if auth["status"] != want || auth["auth_required"] != (want == "unauthenticated") {
			t.Fatal(auth)
		}
	}
	check(loggedOut, false, "recovering")
	check(workerUnavailable, false, "recovering")
	check(loggedOut, false, "recovering")
	check(loggedOut, false, "unauthenticated")
	check(loggedOut, false, "unauthenticated")
	check(workerUnavailable, false, "recovering")
	check(loggedOut, true, "recovering")
	check(loggedOut, true, "unauthenticated")
}

func TestIdleCheckSkipsBusyAndExternalSessions(t *testing.T) {
	c := authConfig(t, "fail")
	c.authKeepalive = 10 * time.Millisecond
	c.nativeACP = &command{bin: filepath.Join(t.TempDir(), "absent")}
	g := fixtureFor(t, c).g
	g.sessions["pending"] = &session{turn: &turn{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); g.maintainLogin(ctx, g.worker) }()
	defer func() { cancel(); <-done }()
	time.Sleep(50 * time.Millisecond)
	g.mu.Lock()
	started := !g.lastAuthCheck.IsZero()
	delete(g.sessions, "pending")
	g.mu.Unlock()
	if started {
		t.Fatal("probe ran while a tool turn was pending")
	}
	until(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.authCheckError != "" })
	if s := adminCall(t, g, "status", nil, 200, ""); asObj(s["auth"])["status"] != "authenticated" || asObj(s["acp"])["alive"] != true {
		t.Fatal(s)
	}
	c.credentialMode = "external"
	if newGateway(ctx, c).authKeepaliveEnabled() {
		t.Fatal("external credentials must remain under host control")
	}
	c.credentialMode = "managed"
	c.authKeepalive = 0
	if newGateway(ctx, c).authKeepaliveEnabled() {
		t.Fatal("disabled interval still schedules checks")
	}
}
