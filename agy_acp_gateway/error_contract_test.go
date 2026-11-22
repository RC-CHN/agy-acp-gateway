package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func rpcReject(p *peer, f frame, code int, message string) {
	_ = p.send(obj{"jsonrpc": "2.0", "id": f.ID, "error": obj{"code": code, "message": message, "data": obj{"token": "secret-canary", "code": 429}}})
}

func TestPublicLocalErrors(t *testing.T) {
	c := defaults()
	c.key = "fixture"
	c.bodyLimit = 110
	g := fixtureFor(t, c).g
	for _, tc := range []struct {
		path, method, body, key string
		status                  int
		code, param             string
	}{
		{"responses", "POST", `{"stream":true}`, "bad", 401, "invalid_api_key", ""},
		{"responses", "POST", "{", "fixture", 400, "invalid_json", ""},
		{"responses", "POST", strings.Repeat(" ", 120) + "{}", "fixture", 413, "request_too_large", ""},
		{"responses", "POST", `{"stream":"true"}`, "fixture", 400, "invalid_parameter", "stream"},
		{"responses", "POST", `{"reasoning":{"effort":false}}`, "fixture", 400, "invalid_parameter", "reasoning.effort"},
		{"chat/completions", "POST", `{"messages":[]}`, "fixture", 400, "invalid_parameter", "messages"},
		{"responses", "POST", `{"input":[{"type":"function_call_output","call_id":"unknown","output":"ok"}]}`, "fixture", 400, "invalid_tool_history", "input"},
		{"responses", "POST", `{"input":"ok","previous_response_id":"gone"}`, "fixture", 400, "previous_response_unavailable", "previous_response_id"},
		{"responses", "POST", `{"input":"ok","model":"bad"}`, "fixture", 400, "invalid_model", "model"},
		{"responses", "GET", "", "fixture", 405, "method_not_allowed", ""},
		{"models", "POST", "{}", "fixture", 405, "method_not_allowed", ""},
		{"unknown", "POST", "{}", "fixture", 404, "not_found", ""},
	} {
		w := invoke(context.Background(), g, tc.method, "/v1/"+tc.path, tc.body, http.Header{"Authorization": {"Bearer " + tc.key}})
		var v obj
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		e := asObj(v["error"])
		param, present := e["param"]
		if w.Code != tc.status || e["code"] != tc.code || !present || str(param) != tc.param || e["upstream"] != nil || w.Header().Get("X-Request-Id") == "" || !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s %s: %d %s", tc.path, tc.code, w.Code, w.Body.String())
		}
	}
}

func TestACPErrorsAtPublicEndpoints(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, rpc := range []int{0, -32800, -32603, -39991} {
				name := fmt.Sprintf("responses=%t/stream=%t/rpc=%d", responses, stream, rpc)
				t.Run(name, func(t *testing.T) {
					c := defaults()
					g := fixtureWithACP(t, c, func(p *peer, f frame) bool {
						if f.Method != "session/prompt" {
							return false
						}
						p.notify("session/update", obj{"sessionId": f.Params["sessionId"], "update": obj{"sessionUpdate": "agent_message_chunk", "content": obj{"type": "text", "text": "partial output"}}})
						if rpc == 0 {
							p.reply(f.ID, obj{"stopReason": "cancelled"})
						} else {
							rpcReject(p, f, rpc, "secret-canary")
						}
						return true
					}).g
					path := "/v1/chat/completions"
					b := chat(user("ok"))
					if responses {
						path = "/v1/responses"
						b = obj{"input": "ok"}
					}
					b["stream"] = stream
					w := invoke(context.Background(), g, "POST", path, enc(b), nil)
					if g.usage.total.Load() != 1 || g.usage.failed.Load() != 1 {
						t.Fatal(g.usage.snapshot())
					}
					wire := w.Body.String()
					if strings.Contains(wire, "secret-canary") || w.Header().Get("X-Request-Id") == "" {
						t.Fatal(wire)
					}
					want := "acp_request_failed"
					if rpc == 0 || rpc == -32800 {
						want = "acp_request_cancelled"
					}
					var e obj
					if !stream {
						var v obj
						_ = json.Unmarshal([]byte(wire), &v)
						e = asObj(v["error"])
						if w.Code != 502 {
							t.Fatal(w.Code, wire)
						}
					} else {
						if w.Code != 200 || strings.Contains(wire, "response.completed") || strings.Contains(wire, `"finish_reason":"stop"`) {
							t.Fatal(wire)
						}
						terminals := 0
						next := 0
						id := ""
						for _, line := range strings.Split(wire, "\n") {
							if !strings.HasPrefix(line, "data: {") {
								continue
							}
							var v obj
							if err := json.Unmarshal([]byte(line[6:]), &v); err != nil {
								t.Fatal(err)
							}
							if responses {
								if v["sequence_number"] != float64(next) {
									t.Fatal(wire)
								}
								next++
							}
							if v["type"] == "response.created" {
								id = str(asObj(v["response"])["id"])
							}
							if v["type"] == "response.failed" {
								terminals++
								res := asObj(v["response"])
								gx := asObj(v["gateway_error"])
								e = asObj(gx["error"])
								if res["id"] != id || id == "" || res["status"] != "failed" || asObj(res["error"])["code"] != "server_error" || gx["status"] != 502.0 || !strings.Contains(enc(res["output"]), "partial output") {
									t.Fatal(wire)
								}
							} else if !responses && v["error"] != nil {
								terminals++
								e = asObj(v["error"])
							}
						}
						if terminals != 1 || (!responses && (strings.Count(wire, "[DONE]") != 1 || !strings.HasSuffix(wire, "data: [DONE]\n\n"))) || (responses && strings.Contains(wire, "[DONE]")) {
							t.Fatal(wire)
						}
					}
					if e["code"] != want || (rpc != 0 && asObj(e["upstream"])["acp_code"] != float64(rpc)) || (rpc == 0 && e["upstream"] != nil) {
						t.Fatal(wire)
					}
				})
			}
		}
	}
}

func TestRPCContextAndAuthentication(t *testing.T) {
	t.Run("active auth then wait", func(t *testing.T) {
		g := fixtureWithACP(t, defaults(), func(p *peer, f frame) bool {
			if f.Method == "session/new" {
				rpcReject(p, f, -32000, "Authentication required")
				return true
			}
			return false
		}).g
		for _, path := range []string{"responses", "chat/completions", "models"} {
			method, body := "POST", enc(obj{"input": "hi", "stream": true})
			if path == "chat/completions" {
				body = enc(chat(user("hi")))
			}
			if path == "models" {
				method, body = "GET", ""
			}
			w := invoke(context.Background(), g, method, "/v1/"+path, body, nil)
			var v obj
			_ = json.Unmarshal(w.Body.Bytes(), &v)
			e := asObj(v["error"])
			up := asObj(e["upstream"])
			if w.Code != 503 || e["code"] != "acp_auth_required" || up["acp_code"] != -32000.0 || up["acp_message"] != "Authentication required" {
				t.Fatal(w.Body.String())
			}
		}
		for _, path := range []string{"/health", "/ready"} {
			w := invoke(context.Background(), g, "GET", path, "", nil)
			if ((path == "/ready") != (w.Code == 503)) || !strings.Contains(w.Body.String(), `"auth_required":true`) {
				t.Fatal(w.Body.String())
			}
		}
	})
	t.Run("stale continuation", func(t *testing.T) {
		var n atomic.Int32
		g := fixtureWithACP(t, defaults(), func(p *peer, f frame) bool {
			if f.Method == "session/prompt" && n.Add(1) == 2 {
				rpcReject(p, f, -32602, "Session not found: "+str(f.Params["sessionId"]))
				return true
			}
			return false
		}).g
		first := postJSON(t, g, "responses", obj{"input": "first"}, 200)
		next := obj{"previous_response_id": first["id"], "input": "next"}
		e := asObj(postJSON(t, g, "responses", next, 400)["error"])
		if e["code"] != "previous_response_unavailable" || asObj(e["upstream"])["acp_code"] != -32602.0 {
			t.Fatal(e)
		}
		if e := asObj(postJSON(t, g, "responses", next, 400)["error"]); e["upstream"] != nil {
			t.Fatal(e)
		}
	})
	for _, tc := range []struct {
		method, model, config, value, sid, msg, want string
		code                                         int
	}{
		{"session/set_model", "named", "", "", "", "Unknown model ID: named", "invalid_model", -32602},
		{"session/set_model", "named", "", "", "", "Unknown model ID: different", "acp_request_failed", -32602},
		{"session/set_config_option", "", "thought_level", "high", "", "Thinking level \"high\" is not offered for model \"named\"", "invalid_reasoning_effort", -32602},
		{"session/set_config_option", "", "thought_level", "high", "", "Unknown config option: thought_level", "acp_protocol_error", -32602},
		{"session/prompt", "", "", "", "abc", "Session not found: fake", "acp_request_failed", -32602},
		{"session/prompt", "", "", "", "abc", "Session not found: abc", "previous_response_unavailable", -32002},
		{"session/prompt", "", "", "", "", "Session not found: ", "acp_protocol_error", -32602},
		{"session/prompt", "", "", "", "abc", "Empty prompt", "acp_protocol_error", -32602},
		{"initialize", "", "", "", "", "secret-canary", "acp_protocol_error", -32700},
		{"initialize", "", "", "", "", "Invalid request", "acp_protocol_error", -32600},
		{"initialize", "", "", "", "", "secret-canary", "acp_protocol_error", -32601},
	} {
		r := &rpcFailure{wire: &wireError{Code: &tc.code, Message: &tc.msg}, method: tc.method, model: tc.model, configID: tc.config, value: tc.value, sessionID: tc.sid}
		f := r.classify(request{responses: true, clientModel: true}, "abc", "named")
		if f.code != tc.want || strings.Contains(enc(errorBody(f)), "secret-canary") {
			t.Fatal(tc, f)
		}
	}
}

func TestFinalRPCFrameThenEOF(t *testing.T) {
	for _, tc := range []struct{ line, code string }{
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"Authentication required"}}`, "acp_auth_required"},
		{`{"jsonrpc":"2.0","id":1,"error":{"message":"missing code"}}`, "acp_protocol_error"},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":0}}`, "acp_protocol_error"},
		{`{"jsonrpc":"2.0","id":1,"result":null}`, "acp_protocol_error"},
		{`{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":0,"message":""}}`, "acp_protocol_error"},
		{"invalid JSON", "acp_protocol_error"}, {"", "worker_disconnected"},
	} {
		read, write := io.Pipe()
		p := newPeer(write, nil)
		go func() {
			defer read.Close()
			var v obj
			_ = json.NewDecoder(read).Decode(&v)
			p.read(strings.NewReader(tc.line))
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := p.call(ctx, "initialize", nil)
		cancel()
		_ = write.Close()
		if err == nil || publicFailure(err).code != tc.code {
			t.Fatalf("%s -> %v", tc.line, err)
		}
	}
}

func TestRetryAfterOnPublicWire(t *testing.T) {
	for _, source := range []int{403, 429, 503} {
		f := observedError(obj{"stopReason": "end_turn", "gateway_observed_http_status": float64(source), "gateway_observed_retry_after_s": 0.0, "gateway_observed_provider_status": "SECRET_VALUE"})
		rec := httptest.NewRecorder()
		writeFailure(rec, f)
		w := rec.Header()
		if (w.Get("Retry-After") == "0") != (source != 403) || strings.Contains(enc(errorBody(f)), "SECRET_VALUE") {
			t.Fatal(w, errorBody(f))
		}
	}
}

func TestAdmissionErrorCodes(t *testing.T) {
	c := defaults()
	c.maxTurns = 1
	g := fixtureFor(t, c).g
	q := request{key: "one", history: []message{{Role: "user", Blocks: []obj{{"type": "text", "text": "one"}}}}}
	l, err := g.acquire(&q)
	if err != nil {
		t.Fatal(err)
	}
	defer g.drop(l.s)
	for _, tc := range []struct{ key, code string }{{"one", "session_busy"}, {"two", "gateway_capacity_exceeded"}} {
		q.key = tc.key
		_, err := g.acquire(&q)
		if err == nil || publicFailure(err).code != tc.code || publicFailure(err).retryAfter != nil {
			t.Fatalf("%s: %v", tc.key, err)
		}
	}
}

func TestRealAuthWaitAndRecovery(t *testing.T) {
	bin := os.Getenv("AGY_REAL_BINARY")
	if bin == "" {
		t.Skip("requires loopback-only native ACP fixture")
	}
	if err := offlineNamespace(); err != nil {
		t.Fatal(err)
	}
	cc := &ccpaCapture{}
	up := httptest.NewServer(cc)
	defer up.Close()
	c := defaults()
	c.cwd = t.TempDir()
	c.key = "fixture"
	c.mode = "live"
	home := t.TempDir()
	c.credentialHome = home
	cmd, err := offlineCredentialCommand(bin, up.URL, "", home, c.cwd, false)
	if err != nil {
		t.Fatal(err)
	}
	c.nativeACP = &cmd
	var mode atomic.Value
	mode.Store("gated")
	var realLogin atomic.Bool
	realLogin.Store(true)
	c.loginCommand = func() *exec.Cmd {
		if !realLogin.Load() {
			login := testLogin(home, mode.Load().(string))()
			if mode.Load().(string) == "ok" {
				login.Env = append(login.Env, "GATEWAY_FIXTURE_EMAIL=c@example.invalid")
			}
			return login
		}
		login := exec.Command(cmd.bin, "acp", "--login")
		login.Env, login.Dir = cmd.env, cmd.dir
		return login
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := newGateway(ctx, c)
	done := make(chan struct{})
	go func() { defer close(done); supervise(ctx, cmd, g) }()
	defer func() { cancel(); g.auth.shutdown(); <-done }()
	auth := http.Header{"Authorization": {"Bearer fixture"}}
	probe := func(waiting bool) {
		t.Helper()
		limit := time.Now().Add(12 * time.Second)
		for time.Now().Before(limit) {
			w := invoke(ctx, g, "GET", "/ready", "", nil)
			var v obj
			_ = json.Unmarshal(w.Body.Bytes(), &v)
			if v["auth_required"] == waiting && (w.Code == 503) == waiting {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for auth_required=%v", waiting)
	}
	probe(true)
	w := invoke(ctx, g, "GET", "/v1/models", "", auth)
	var v obj
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	e := asObj(v["error"])
	if w.Code != 503 || e["code"] != "acp_auth_required" || asObj(e["upstream"])["acp_code"] != -32000.0 {
		t.Fatalf("native bootstrap: %d %s", w.Code, w.Body)
	}
	w = invoke(ctx, g, "GET", "/v1/models", "", nil)
	if w.Code != 401 || strings.Contains(w.Body.String(), "acp_code") {
		t.Fatal(w.Body.String())
	}
	if status := adminCall(t, g, "status", nil, 200, ""); asObj(status["auth"])["status"] != "unauthenticated" {
		t.Fatal(status)
	}
	if w := invoke(ctx, g, "GET", "/health", "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	v = adminCall(t, g, "auth/start", obj{}, 200, "")
	if !officialAuthURL(str(v["auth_url"])) {
		t.Fatal("real CLI returned no official link")
	}
	g.auth.mu.Lock()
	pid := g.auth.current.pid
	g.auth.mu.Unlock()
	adminCall(t, g, "auth/cancel", obj{"login_id": v["login_id"]}, 200, "")
	if syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("real login child survived cancel")
	}
	realLogin.Store(false)
	v = adminCall(t, g, "auth/start", obj{}, 200, "")
	if err = os.WriteFile(filepath.Join(home, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	adminCall(t, g, "auth/complete", obj{"login_id": v["login_id"], "code": "synthetic"}, 200, "")
	probe(false)
	// A second transaction starts on a ready account. Cancel leaves it serving.
	v = adminCall(t, g, "auth/start", obj{}, 200, "")
	if s := adminCall(t, g, "status", nil, 200, ""); asObj(s["auth"])["email"] != "b@example.invalid" || asObj(s["acp"])["alive"] != true {
		t.Fatal(s)
	}
	adminCall(t, g, "auth/cancel", obj{"login_id": v["login_id"]}, 200, "")
	probe(false)

	w = invoke(ctx, g, "GET", "/v1/models", "", auth)
	if w.Code != 200 || strings.Contains(w.Body.String(), "acp_auth_required") {
		t.Fatal(w.Code, w.Body.String())
	}
	// A successful CLI exit must leave freshly written credentials. A failed
	// switch recovers the old identity only after a fresh native catalog probe.
	mode.Store("no-file")
	v = adminCall(t, g, "auth/start", obj{}, 200, "")
	adminCall(t, g, "auth/complete", obj{"login_id": v["login_id"], "code": "synthetic"}, 500, "credential_update_failed")
	probe(false)
	w = invoke(ctx, g, "POST", "/v1/responses", enc(obj{"input": "before switch"}), auth)
	var prior obj
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &prior) != nil || prior["id"] == nil {
		t.Fatal(w.Code)
	}
	mode.Store("ok")
	v = adminCall(t, g, "auth/start", obj{}, 200, "")
	result := adminCall(t, g, "auth/complete", obj{"login_id": v["login_id"], "code": "synthetic"}, 200, "")
	if result["email"] != "c@example.invalid" {
		t.Fatal(result)
	}
	probe(false)
	w = invoke(ctx, g, "POST", "/v1/responses", enc(obj{"previous_response_id": prior["id"], "input": "after switch"}), auth)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "previous_response_unavailable") {
		t.Fatal(w.Code, w.Body)
	}
	adminCall(t, g, "auth/logout", obj{}, 200, "")
	probe(true)

}

// Exercise Cmd.Wait, stdout draining and session retirement together. A complete
// error reply immediately before process exit still belongs to its HTTP caller.
func TestWorkerExitsAfterPromptError(t *testing.T) {
	if os.Getenv("AGY_FINAL_ERROR_FIXTURE") == "1" {
		dec := json.NewDecoder(os.Stdin)
		out := json.NewEncoder(os.Stdout)
		for {
			var f frame
			if dec.Decode(&f) != nil {
				os.Exit(0)
			}
			resp := obj{"jsonrpc": "2.0", "id": f.ID}
			switch f.Method {
			case "initialize":
				resp["result"] = obj{"protocolVersion": 1}
			case "session/new":
				resp["result"] = obj{"sessionId": uid("offline-"), "models": obj{"currentModelId": "mock", "availableModels": []obj{{"modelId": "mock"}}}}
			case "session/prompt":
				resp["error"] = obj{"code": -32603, "message": "private-error-canary"}
				if out.Encode(resp) != nil {
					os.Exit(1)
				}
				os.Exit(0)
			default:
				resp["result"] = obj{}
			}
			if out.Encode(resp) != nil {
				os.Exit(1)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := defaults()
	c.cwd = t.TempDir()
	g := newGateway(ctx, c)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = runProcess(ctx, command{bin: os.Args[0], args: []string{"-test.run=^TestWorkerExitsAfterPromptError$"}, env: []string{"AGY_FINAL_ERROR_FIXTURE=1", "GORACE=atexit_sleep_ms=0"}, dir: c.cwd}, g)
	}()
	defer func() { cancel(); <-ended }()
	for len(g.modelsList()) == 0 {
		select {
		case <-ended:
			t.Fatal("worker exited during setup")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	e := asObj(postJSON(t, g, "responses", obj{"input": "offline-final-error"}, 502)["error"])
	if e["code"] != "acp_request_failed" || asObj(e["upstream"])["acp_code"] != -32603.0 || strings.Contains(enc(e), "private-error-canary") {
		t.Fatal(e)
	}
}
