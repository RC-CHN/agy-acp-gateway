package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

type fixture struct {
	g      *gateway
	mu     sync.Mutex
	frames []frame
	mock   *peer
}

func fixtureFor(t *testing.T, c config) *fixture {
	return fixtureWithACP(t, c, nil)
}
func fixtureWithACP(t *testing.T, c config, intercept func(*peer, frame) bool) *fixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.cwd = t.TempDir()
	c.bridgeURL = "http://127.0.0.1:12345"
	g := newGateway(ctx, c)
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	p := newPeer(w1, g.incoming)
	client := &http.Client{Transport: handlerTransport{http.HandlerFunc(g.mcp)}}
	m := &mockACP{sessions: map[string]*mockSession{}, http: client}
	f := &fixture{g: g}
	child := newPeer(w2, func(p *peer, v frame) {
		f.mu.Lock()
		f.frames = append(f.frames, v)
		f.mu.Unlock()
		if intercept == nil || !intercept(p, v) {
			m.handle(p, v)
		}
	})
	f.mock = child
	g.worker = p
	g.catalog = []string{"mock", "mock-alt"}
	go p.read(r2)
	go child.read(r1)
	t.Cleanup(func() {
		cancel()
		g.mu.Lock()
		for _, s := range g.sessions {
			g.removeLocked(s)
		}
		g.mu.Unlock()
		w1.Close()
		w2.Close()
		r1.Close()
		r2.Close()
	})
	return f
}
func (f *fixture) framesFor(method string) []frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []frame
	for _, v := range f.frames {
		if v.Method == method {
			out = append(out, v)
		}
	}
	return out
}
func invoke(ctx context.Context, h http.Handler, method, path, body string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	for k, vs := range headers {
		r.Header[k] = vs
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func postJSON(t *testing.T, g *gateway, path string, b obj, code int) obj {
	t.Helper()
	w := invoke(context.Background(), g, "POST", "/v1/"+path, enc(b), nil)
	if w.Code != code {
		t.Fatalf("%s: HTTP %d (want %d) %s", path, w.Code, code, w.Body.String())
	}
	var v obj
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func user(s string) obj    { return obj{"role": "user", "content": s} }
func chat(msgs ...obj) obj { return obj{"model": "mock", "messages": msgs} }
func choice(v obj) obj     { return asObj(arr(v["choices"])[0]) }
func assistant(v obj) obj  { return asObj(choice(v)["message"]) }
func specs(names ...string) []obj {
	var a []obj
	for _, name := range names {
		a = append(a, obj{"type": "function", "name": name, "parameters": obj{"type": "object"}})
	}
	return a
}
func chatTools(names ...string) []obj {
	var a []obj
	for _, t := range specs(names...) {
		a = append(a, obj{"type": "function", "function": obj{"name": t["name"], "parameters": t["parameters"]}})
	}
	return a
}
func until(t *testing.T, check func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(end) {
			t.Fatal("timed out waiting for state")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestChatHistoryAndConfig(t *testing.T) {
	c := defaults()
	c.models = map[string]string{"alias": "mock-alt"}
	f := fixtureFor(t, c)
	sys := obj{"role": "developer", "content": "system-marker"}
	u := user("hello")
	q := chat(sys, u)
	q["model"] = "alias"
	q["reasoning_effort"] = "high"
	v := postJSON(t, f.g, "chat/completions", q, 200)
	a := assistant(v)
	if a["content"] != "echo: hello" || a["reasoning_content"] != "mock reasoning" || asObj(asObj(v["usage"])["prompt_tokens_details"])["cached_tokens"] != float64(3) {
		t.Fatal(v)
	}
	q["messages"] = []obj{sys, u, a, user("next")}
	postJSON(t, f.g, "chat/completions", q, 200)
	if len(f.framesFor("session/new")) != 1 || len(f.framesFor("session/set_model")) != 1 || len(f.framesFor("session/set_config_option")) != 1 {
		t.Fatal("linear session was not reused")
	}
	p := f.framesFor("session/prompt")
	if len(arr(p[1].Params["prompt"])) != 1 || !strings.Contains(enc(f.framesFor("session/new")[0].Params["_meta"]), "system-marker") {
		t.Fatal("unexpected ACP history or system instructions")
	}
	q["messages"] = []obj{sys, u, obj{"role": "assistant", "content": "edited"}, user("branch")}
	postJSON(t, f.g, "chat/completions", q, 200)
	if len(f.framesFor("session/new")) != 2 || !strings.Contains(enc(f.framesFor("session/prompt")[2].Params["prompt"]), "edited") {
		t.Fatal("edited history missing on rebuild")
	}
	q["reasoning_effort"] = "unknown"
	postJSON(t, f.g, "chat/completions", q, 400)
}
func TestParallelToolRoundTrips(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(fmt.Sprint(responses), func(t *testing.T) {
			f := fixtureFor(t, defaults())
			path := "chat/completions"
			q := chat(user("tools: weather and time"))
			q["tools"] = chatTools("weather", "clock")
			if responses {
				path = "responses"
				q = obj{"input": "tools: weather and time", "tools": specs("weather", "clock")}
			}
			first := postJSON(t, f.g, path, q, 200)
			if f.g.usage.input.Load() != 0 {
				t.Fatal("premature usage")
			}
			var calls []any
			var history []obj
			if responses {
				for _, raw := range arr(first["output"]) {
					if asObj(raw)["type"] == "function_call" {
						calls = append(calls, raw)
					}
				}
			} else {
				if choice(first)["finish_reason"] != "tool_calls" {
					t.Fatal(first)
				}
				a := assistant(first)
				calls = arr(a["tool_calls"])
				history = []obj{user("tools: weather and time"), a}
			}
			if len(calls) != 2 {
				t.Fatal(first)
			}
			var results []obj
			for i := 1; i >= 0; i-- {
				x := asObj(calls[i])
				if responses {
					results = append(results, obj{"type": "function_call_output", "call_id": x["call_id"], "output": "ok-" + str(x["name"])})
				} else {
					results = append(results, obj{"role": "tool", "tool_call_id": x["id"], "content": "ok-" + str(asObj(x["function"])["name"])})
				}
			}
			next := func(results []obj) obj {
				if responses {
					return obj{"previous_response_id": first["id"], "input": results}
				}
				b := chat(append(slices.Clone(history), results...)...)
				b["tools"] = q["tools"]
				return b
			}
			postJSON(t, f.g, path, next(results[:1]), 400)
			postJSON(t, f.g, path, next([]obj{results[0], results[0]}), 400)
			v := postJSON(t, f.g, path, next(results), 200)
			if f.g.usage.input.Load() != 12 || f.g.usage.total.Load() != 4 || f.g.usage.failed.Load() != 2 {
				t.Fatal(f.g.usage.snapshot())
			}
			if !strings.Contains(enc(v), "ok-weather") || !strings.Contains(enc(v), "ok-clock") || len(f.framesFor("session/prompt")) != 1 {
				t.Fatal(v)
			}
			if responses {
				postJSON(t, f.g, path, next(results), 400)
			} else {
				// Re-sending a valid complete Chat Completions tool transcript rebuilds and replays cleanly.
				replayed := postJSON(t, f.g, path, next(results), 200)
				if !strings.Contains(enc(replayed), "ok-weather") || !strings.Contains(enc(replayed), "ok-clock") || len(f.framesFor("session/new")) != 2 {
					t.Fatal(replayed)
				}
			}
		})
	}
}
func TestResponsesBranchAndInstructions(t *testing.T) {
	f := fixtureFor(t, defaults())
	a := postJSON(t, f.g, "responses", obj{"input": "one", "instructions": "first only"}, 200)
	b := postJSON(t, f.g, "responses", obj{"input": "two", "previous_response_id": a["id"]}, 200)
	if len(f.framesFor("session/new")) != 2 || strings.Contains(enc(f.framesFor("session/new")[1].Params["_meta"]), "first only") {
		t.Fatal("old instructions leaked")
	}
	postJSON(t, f.g, "responses", obj{"input": "three", "previous_response_id": a["id"]}, 400)
	postJSON(t, f.g, "responses", obj{"input": "three", "previous_response_id": b["id"]}, 200)
}
func events(t *testing.T, b []byte) []obj {
	t.Helper()
	var out []obj
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "data: ") && l != "data: [DONE]" {
			var evt obj
			if err := json.Unmarshal([]byte(l[6:]), &evt); err != nil {
				t.Fatal(err)
			}
			out = append(out, evt)
		}
	}
	return out
}
func TestStream(t *testing.T) {
	f := fixtureFor(t, defaults())
	q := chat(user("length: hi"))
	q["stream"] = true
	q["stream_options"] = obj{"include_usage": true}
	w := invoke(context.Background(), f.g, "POST", "/v1/chat/completions", enc(q), nil)
	es := events(t, w.Body.Bytes())
	if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") || choice(es[len(es)-2])["finish_reason"] != "length" || es[len(es)-1]["usage"] == nil {
		t.Fatal(w.Body)
	}
	for _, s := range []string{"hello", "length: reply", "tools: one"} {
		q := obj{"stream": true, "input": s}
		if s == "tools: one" {
			q["tools"] = specs("one")
		}
		w := invoke(context.Background(), f.g, "POST", "/v1/responses", enc(q), nil)
		es := events(t, w.Body.Bytes())
		var names []string
		for i, v := range es {
			if v["sequence_number"] != float64(i) {
				t.Fatal(v)
			}
			names = append(names, str(v["type"]))
		}
		end := "response.completed"
		if strings.HasPrefix(s, "length") {
			end = "response.incomplete"
		}
		if len(names) < 6 || names[0] != "response.created" || names[len(names)-1] != end || !slices.Contains(names, "response.reasoning_summary_text.delta") {
			t.Fatal(names)
		}
		if s == "tools: one" && !slices.Contains(names, "response.function_call_arguments.done") {
			t.Fatal(names)
		}
	}
}
func TestLimitsAuthImagesAndErrors(t *testing.T) {
	c := defaults()
	c.key = "secret"
	c.bodyLimit = 700
	c.cors = "https://example.invalid"
	f := fixtureFor(t, c)
	h := http.Header{"Authorization": []string{"Bearer secret"}}
	for _, tc := range []struct {
		method, path, body string
		headers            http.Header
		code               int
	}{
		{"GET", "/health", "", nil, 200}, {"GET", "/ready", "", nil, 200}, {"GET", "/v1/models", "", nil, 401}, {"GET", "/v1/models", "", h, 200}, {"GET", "/unknown", "", h, 404}, {"POST", "/v1/responses", "{", h, 400}, {"POST", "/v1/responses", "{}{}", h, 400}, {"POST", "/v1/responses", strings.Repeat(" ", 720) + "{}", h, 413}, {"POST", "/v1/responses", enc(obj{"input": "ok", "model": "missing"}), h, 400}, {"OPTIONS", "/v1/responses", "", http.Header{"Origin": []string{"https://example.invalid"}}, 204},
	} {
		w := invoke(context.Background(), f.g, tc.method, tc.path, tc.body, tc.headers)
		if w.Code != tc.code {
			t.Errorf("%s %s got %d, want %d", tc.method, tc.path, w.Code, tc.code)
		}
	}
	for _, u := range []string{"data:image/png;base64,aGk=", "https://example.invalid/x", "data:text/plain;base64,YQ==", "data:image/png;base64,broken"} {
		q := obj{"input": []obj{{"role": "user", "content": []obj{{"type": "input_image", "image_url": u}}}}}
		w := invoke(context.Background(), f.g, "POST", "/v1/responses", enc(q), h)
		if (w.Code == 200) != (u == "data:image/png;base64,aGk=") {
			t.Fatal(w.Code, w.Body)
		}
	}
	n := maxImageBytes
	_, err := content([]any{obj{"type": "input_image", "image_url": "data:image/png;base64,YQ=="}}, &n)
	if err == nil {
		t.Fatal("image quota")
	}
	bad := chat(obj{"role": "assistant", "tool_calls": []obj{{"id": "x", "function": obj{"name": "foo", "arguments": "{bad"}}}}, obj{"role": "tool", "tool_call_id": "x", "content": "ok"}, user("next"))
	w := invoke(context.Background(), f.g, "POST", "/v1/chat/completions", enc(bad), h)
	if w.Code != 200 || !strings.Contains(enc(f.framesFor("session/prompt")), "_raw") {
		t.Fatal(w.Body)
	}
}
func TestTimeoutEvictionAndDisconnect(t *testing.T) {
	c := defaults()
	c.maxSessions = 1
	c.maxTurns = 1
	c.toolTimeout = 110 * time.Millisecond
	c.batch = 2 * time.Millisecond
	c.idle = time.Minute
	f := fixtureFor(t, c)
	a := postJSON(t, f.g, "responses", obj{"input": "a"}, 200)
	postJSON(t, f.g, "responses", obj{"input": "b"}, 200)
	postJSON(t, f.g, "responses", obj{"input": "x", "previous_response_id": a["id"]}, 400)
	z := postJSON(t, f.g, "responses", obj{"input": "tools: x", "tools": specs("x")}, 200)
	postJSON(t, f.g, "responses", obj{"input": "other"}, 429)
	if invoke(context.Background(), f.g, "GET", "/ready", "", nil).Code != 503 {
		t.Fatal("overloaded ready")
	}
	until(t, func() bool { f.g.mu.Lock(); defer f.g.mu.Unlock(); return len(f.g.sessions) == 0 })
	postJSON(t, f.g, "responses", obj{"input": "x", "previous_response_id": z["id"]}, 400)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		invoke(ctx, f.g, "POST", "/v1/chat/completions", enc(chat(user("wait: disconnect"))), nil)
		close(done)
	}()
	until(t, func() bool { return len(f.framesFor("session/prompt")) >= 4 })
	postJSON(t, f.g, "chat/completions", chat(user("wait: disconnect")), 429)
	cancel()
	<-done
	until(t, func() bool { return len(f.framesFor("session/cancel")) >= 2 })
	postJSON(t, f.g, "responses", obj{"input": "idle"}, 200)
	f.g.mu.Lock()
	for _, s := range f.g.sessions {
		s.last = time.Now().Add(-2 * c.idle)
	}
	f.g.mu.Unlock()
	f.g.reap()
	if len(f.g.modelsList()) == 0 {
		t.Fatal("catalog evicted")
	}
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	if len(f.g.sessions) != 0 {
		t.Fatal("expired session retained")
	}
}
func TestPermissionAndMCPCapabilities(t *testing.T) {
	f := fixtureFor(t, defaults())
	first := postJSON(t, f.g, "responses", obj{"input": "tools: x", "tools": specs("weather")}, 200)
	f.g.mu.Lock()
	s := f.g.tips[str(first["id"])]
	f.g.mu.Unlock()
	for _, tc := range []struct{ name, kind, want string }{{"write_file", "edit", "deny"}, {"mcp_other_weather", "other", "deny"}, {"mcp_vbridge_missing", "other", "deny"}, {"mcp_vbridge_weather", "edit", "deny"}, {"mcp_vbridge_weather", "other", "allow"}} {
		ctx, stop := context.WithTimeout(context.Background(), time.Second)
		v, err := f.mock.call(ctx, "session/request_permission", obj{"sessionId": s.id, "toolCall": obj{"title": "Run mcp_vbridge_weather", "name": tc.name, "kind": tc.kind, "toolCallId": "id"}, "options": []obj{{"kind": "allow_once", "optionId": "allow"}, {"kind": "reject_once", "optionId": "deny"}}})
		stop()
		if err != nil || asObj(v["outcome"])["optionId"] != tc.want {
			t.Fatal(v, err)
		}
	}
	for _, tc := range []struct {
		token, id, method string
		http              int
		rpcError          bool
	}{{"bad", "", "tools/list", 404, false}, {s.token, "", "tools/list", 404, false}, {s.token, s.mcpID, "unknown", 200, true}, {s.token, s.mcpID, "tools/call", 200, true}, {s.token, "", "server/discover", 200, true}} {
		h := http.Header{}
		h.Set("Mcp-Session-Id", tc.id)
		w := invoke(context.Background(), http.HandlerFunc(f.g.mcp), "POST", "/mcp/"+tc.token, enc(obj{"jsonrpc": "2.0", "id": 1, "method": tc.method, "params": obj{"name": "nope"}}), h)
		if w.Code != tc.http || strings.Contains(w.Body.String(), `"error"`) != tc.rpcError {
			t.Fatal(w.Code, w.Body)
		}
	}
}
func TestMockWorkerHelper(t *testing.T) {
	if os.Getenv("AGY_MOCK_HELPER") != "1" {
		t.Skip("subprocess fixture")
	}
	if path := os.Getenv("AGY_PID_FILE"); path != "" {
		_ = os.WriteFile(path, []byte(fmt.Sprint(os.Getpid())), 0600)
	}
	serveMock(os.Stdin, os.Stdout, &http.Client{})
	os.Exit(0)
}
func TestWorkerCrashRestartAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := defaults()
	c.cwd = t.TempDir()
	g := newGateway(ctx, c)
	file := c.cwd + "/pid"
	cmd := command{bin: os.Args[0], args: []string{"-test.run=^TestMockWorkerHelper$"}, env: []string{"AGY_MOCK_HELPER=1", "AGY_PID_FILE=" + file}, dir: c.cwd}
	stopped := make(chan struct{})
	go func() { supervise(ctx, cmd, g); close(stopped) }()
	until(t, func() bool { return len(g.modelsList()) > 0 })
	pid := func() int { v, _ := os.ReadFile(file); var id int; fmt.Sscan(string(v), &id); return id }
	old := pid()
	done := make(chan int, 1)
	go func() {
		w := invoke(ctx, g, "POST", "/v1/responses", enc(obj{"input": "wait: crash"}), nil)
		done <- w.Code
	}()
	until(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.ids) > 0 })
	if err := syscall.Kill(old, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 502 {
		t.Fatalf("worker crash got %d", code)
	}
	until(t, func() bool { return pid() != old && len(g.modelsList()) > 0 })
	postJSON(t, g, "responses", obj{"input": "recovered"}, 200)
	newPID := pid()
	cancel()
	<-stopped
	if syscall.Kill(newPID, 0) != syscall.ESRCH {
		t.Fatal("worker not reaped")
	}
}

func TestConcurrentSessionsAndToolAbandonment(t *testing.T) {
	c := defaults()
	c.maxTurns = 16
	f := fixtureFor(t, c)
	errs := make(chan string, 12)
	for i := 0; i < 12; i++ {
		go func() {
			msg := fmt.Sprintf("session %d", i)
			w := invoke(context.Background(), f.g, "POST", "/v1/chat/completions", enc(chat(user(msg))), nil)
			var v obj
			_ = json.Unmarshal(w.Body.Bytes(), &v)
			if w.Code != 200 || assistant(v)["content"] != "echo: "+msg {
				errs <- msg
				return
			}
			errs <- ""
		}()
	}
	for i := 0; i < 12; i++ {
		if x := <-errs; x != "" {
			t.Fatal("session isolation: " + x)
		}
	}
	q := chat(user("tools: one"))
	q["tools"] = chatTools("one")
	q["prompt_cache_key"] = "same"
	postJSON(t, f.g, "chat/completions", q, 200)
	q["messages"] = []obj{user("different task")}
	q["tools"] = chatTools("new_tool")
	v := postJSON(t, f.g, "chat/completions", q, 200)
	if assistant(v)["content"] != "echo: different task" {
		t.Fatal(v)
	}
	until(t, func() bool { return len(f.framesFor("session/cancel")) > 0 })
}
func TestMCPExpiredIDAndInputValidation(t *testing.T) {
	for _, s := range []string{"https://127.0.0.1:80", "http://localhost:80", "http://example.invalid:80", "http://127.0.0.1", "http://user@127.0.0.1:80", "http://127.0.0.1:80/path", "http://127.0.0.1:80?other=x"} {
		if endpointOK(s) {
			t.Error("accepted endpoint", s)
		}
	}
	for _, s := range []string{"http://127.0.0.1:80", "http://[::1]:9000"} {
		if !endpointOK(s) {
			t.Error("rejected loopback", s)
		}
	}
	f := fixtureFor(t, defaults())
	for _, q := range []obj{{"tools": specs("x", "x"), "input": "x"}, {"input": []int{1}}, {"input": []obj{{"type": "unknown"}}}, {"input": []obj{{"type": "function_call_output", "output": "x"}}}, {"input": "x", "tools": specs("bad.tool")}} {
		postJSON(t, f.g, "responses", q, 400)
	}
}

func TestToolNamesAndExactJSONNumbers(t *testing.T) {
	f := fixtureFor(t, defaults())
	for _, name := range []string{"get_weather", "Weather-Lookup", "gw_private", strings.Repeat("a", 64)} {
		v := postJSON(t, f.g, "responses", obj{"input": "tools: alias", "tools": specs(name)}, 200)
		var call obj
		for _, x := range arr(v["output"]) {
			if asObj(x)["type"] == "function_call" {
				call = asObj(x)
			}
		}
		if call["name"] != name {
			t.Fatalf("tool alias %q yielded %v", name, call)
		}
		postJSON(t, f.g, "responses", obj{"previous_response_id": v["id"], "input": []obj{{"type": "function_call_output", "call_id": call["call_id"], "output": "ok"}}}, 200)
	}
	orig := `{"key":9223372036854775807}`
	v, err := normalizeCall("id", "foo", orig)
	if err != nil || asObj(v["function"])["arguments"] != orig {
		t.Fatalf("integer rounded: %v", v)
	}
}

func TestNativeCommandCleanAgentDefault(t *testing.T) {
	bin := os.Args[0]
	cwdDefault := t.TempDir()
	cmd, err := nativeCommand(bin, "", cwdDefault)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.args, "--agent") || !slices.Contains(cmd.args, cleanAgentName) {
		t.Fatalf("expected clean agent args, got %v", cmd.args)
	}
	agentFile := filepath.Join(cwdDefault, ".agents", "agents", cleanAgentName+".md")
	raw, err := os.ReadFile(agentFile)
	if err != nil {
		t.Fatalf("clean agent file missing: %v", err)
	}
	content := string(raw)
	for _, want := range []string{"name: " + cleanAgentName, "mainAgent: true", "excludeDefaultComponents: true", "name: vbridge"} {
		if !strings.Contains(content, want) {
			t.Fatalf("clean agent file missing %q:\n%s", want, content)
		}
	}
	if !strings.HasSuffix(content, "\n---\n") {
		t.Fatalf("expected empty prompt body after frontmatter, got:\n%s", content)
	}

	cwdBuiltin := t.TempDir()
	cmd, err = nativeCommand(bin, "default", cwdBuiltin)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cmd.args, "--agent") {
		t.Fatalf("unexpected --agent for default opt-in: %v", cmd.args)
	}
	if _, err := os.Stat(filepath.Join(cwdBuiltin, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .agents dir for default opt-in")
	}

	cwdCustom := t.TempDir()
	cmd, err = nativeCommand(bin, "custom-a", cwdCustom)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.args, "--agent") || !slices.Contains(cmd.args, "custom-a") {
		t.Fatalf("expected custom agent args, got %v", cmd.args)
	}
	if _, err := os.Stat(filepath.Join(cwdCustom, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("unexpected .agents dir for explicit custom agent")
	}
}

func TestSoftBlockResidualToolsPrompt(t *testing.T) {
	if got := effectiveSystemPrompt("", false, true); got != softBlockNoToolsPrompt || strings.Contains(got, "list_resources") {
		t.Fatalf("unexpected no-tools guard: %q", got)
	}
	if got := effectiveSystemPrompt("client-sys", false, true); got != "client-sys\n\n"+softBlockNoToolsPrompt {
		t.Fatalf("unexpected client+no-tools guard: %q", got)
	}
	if got := effectiveSystemPrompt("", true, true); got != softBlockWithToolsPrompt || !strings.Contains(got, "`manage_task`, `list_resources`, and `read_resource`") {
		t.Fatalf("unexpected with-tools guard: %q", got)
	}
	if got := effectiveSystemPrompt("client-sys", true, true); got != "client-sys\n\n"+softBlockWithToolsPrompt {
		t.Fatalf("unexpected client+with-tools guard: %q", got)
	}
	if got := effectiveSystemPrompt("", true, false); got != "" {
		t.Fatalf("expected empty prompt when soft-block disabled, got %q", got)
	}
	if got := effectiveSystemPrompt("client-sys", true, false); got != "client-sys" {
		t.Fatalf("expected verbatim client prompt when soft-block disabled, got %q", got)
	}

	// Verify session/new sends the expected guard in mock mode too.
	f := fixtureFor(t, defaults())
	postJSON(t, f.g, "responses", obj{"input": "plain"}, 200)
	postJSON(t, f.g, "responses", obj{"input": "tools: check", "tools": specs("weather")}, 200)
	news := f.framesFor("session/new")
	if len(news) < 2 {
		t.Fatalf("expected 2 session/new frames, got %d", len(news))
	}
	getAppend := func(fr frame) string {
		return str(asObj(asObj(asObj(fr.Params["_meta"])["antigravity"])["systemPrompt"])["append"])
	}
	if got := getAppend(news[0]); got != softBlockNoToolsPrompt {
		t.Fatalf("plain session/new append = %q", got)
	}
	if got := getAppend(news[1]); got != softBlockWithToolsPrompt {
		t.Fatalf("tools session/new append = %q", got)
	}

	cOff := defaults()
	cOff.softBlockResidualTools = false
	fOff := fixtureFor(t, cOff)
	postJSON(t, fOff.g, "responses", obj{"input": "plain"}, 200)
	if got := getAppend(fOff.framesFor("session/new")[0]); got != "" {
		t.Fatalf("expected empty append when disabled, got %q", got)
	}
}
