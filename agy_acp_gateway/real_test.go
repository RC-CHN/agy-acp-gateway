package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var offlineReplayValue = regexp.MustCompile(`replay-value-[a-f0-9]+`)

type ccpaCapture struct {
	mu       sync.Mutex
	turns    []obj
	names    []string
	toolName string
	sentinel string
}

func (c *ccpaCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	model := "gemini-2.5-flash"
	tier := obj{"id": "g1-pro-tier", "name": "Offline", "isDefault": true}
	switch {
	case strings.Contains(r.URL.Path, "listExperiments"):
		flags := []obj{}
		for _, kind := range []string{"agent", "mcp"} {
			flags = append(flags, obj{"name": "enable-customization-kind-" + kind, "boolValue": true})
		}
		writeJSON(w, 200, obj{"flags": flags})
	case strings.Contains(r.URL.Path, "loadCodeAssist"):
		writeJSON(w, 200, obj{"currentTier": tier, "allowedTiers": []obj{tier}, "cloudaicompanionProject": "offline-project", "paidTier": tier})
	case strings.Contains(r.URL.Path, "fetchAvailableModels"):
		writeJSON(w, 200, obj{"models": obj{model: obj{"displayName": "Offline", "supportsImages": true, "supportsThinking": true, "recommended": true, "maxTokens": 1000000, "maxOutputTokens": 64000, "model": 312, "apiProvider": 1, "modelProvider": 4}}, "defaultAgentModelId": model, "agentModelSorts": []obj{{"displayName": "Offline", "groups": []obj{{"displayName": "Offline", "modelIds": []string{model}}}}}, "groupedModels": obj{model: obj{"displayName": "Offline", "supportedThinkingLevels": []any{}}}})
	case strings.Contains(r.URL.Path, "streamGenerateContent"):
		var envelope obj
		_ = json.NewDecoder(r.Body).Decode(&envelope)
		req := asObj(envelope["request"])
		part := obj{"text": "offline-final"}
		system := enc(req["systemInstruction"])
		if !strings.Contains(strings.ToLower(system), "conversation title generator") {
			c.mu.Lock()
			c.turns = append(c.turns, req)
			var names []string
			direct := ""
			for _, group := range arr(req["tools"]) {
				for _, raw := range arr(asObj(group)["functionDeclarations"]) {
					d := asObj(raw)
					name := str(d["name"])
					names = append(names, name)
					if name == "run_command" && strings.Contains(enc(req["contents"]), "command-canary") && !strings.Contains(enc(req["contents"]), "functionResponse") {
						args := obj{}
						for _, k := range arr(asObj(d["parameters"])["required"]) {
							switch str(k) {
							case "CommandLine", "command", "commandLine":
								args[str(k)] = "touch " + c.sentinel
							case "Cwd", "cwd", "WorkingDirectory":
								args[str(k)] = filepath.Dir(c.sentinel)
							case "SafeToAutoRun", "ShouldAutoRun":
								args[str(k)] = false
							case "WaitMsBeforeAsync", "waitMsBeforeAsync":
								args[str(k)] = 500
							case "toolSummary", "Description", "explanation":
								args[str(k)] = "Offline permission check"
							default:
								args[str(k)] = "test"
							}
						}
						part = obj{"functionCall": obj{"name": name, "args": args}}
					}
					if name == "write_file" && strings.Contains(enc(req["contents"]), "write-canary") {

						if !strings.Contains(enc(req["contents"]), "functionResponse") {
							part = obj{"functionCall": obj{"name": name, "args": obj{"Path": c.sentinel, "Content": "test"}}}
						}
					}
					if strings.HasSuffix(name, "get_weather") {
						direct = name

					}

				}
			}
			c.names = names
			input := enc(req["contents"])
			if value := offlineReplayValue.FindString(input); value != "" {
				part = obj{"text": "replayed " + value}
			} else if strings.Contains(input, "followup-tool-probe") && direct != "" && !strings.Contains(input, "functionResponse") {
				part = obj{"functionCall": obj{"name": direct, "args": obj{"city": "Suzhou", "toolSummary": "Get second city"}}}
			} else if strings.Contains(input, "weather-probe") && !strings.Contains(input, "functionResponse") {
				if direct != "" {
					part = obj{"functionCall": obj{"name": direct, "args": obj{"city": "Shanghai", "toolSummary": "Get weather"}}}
					c.toolName = direct
				}
			}
			c.mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\r\n\r\n", enc(obj{"response": obj{"candidates": []obj{{"content": obj{"role": "model", "parts": []obj{part}}, "finishReason": "STOP"}}, "usageMetadata": obj{"promptTokenCount": 32, "candidatesTokenCount": 5, "cachedContentTokenCount": 3, "totalTokenCount": 37}}, "traceId": "offline-trace"}))
	default:
		writeJSON(w, 200, obj{})
	}
}

var strippedBuiltInTools = []string{
	"read_file", "run_command", "write_file", "edit_file",
	"search_web", "generate_image", "schedule", "send_message", "ask_question",
}

func declaredToolNames(req obj) []string {
	var names []string
	for _, group := range arr(req["tools"]) {
		for _, raw := range arr(asObj(group)["functionDeclarations"]) {
			if n := str(asObj(raw)["name"]); n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

func systemPromptText(req obj) string {
	var sb strings.Builder
	for _, raw := range arr(asObj(req["systemInstruction"])["parts"]) {
		sb.WriteString(str(asObj(raw)["text"]))
	}
	return sb.String()
}

func assertCleanTurn(t *testing.T, req obj, wantTools []string, allowResourceTools bool) {
	t.Helper()
	names := declaredToolNames(req)
	for _, bad := range strippedBuiltInTools {
		if slices.Contains(names, bad) {
			t.Fatalf("unexpected built-in tool %q in clean mode; tools=%v", bad, names)
		}
	}
	if !allowResourceTools {
		for _, resTool := range []string{"list_resources", "read_resource"} {
			if slices.Contains(names, resTool) {
				t.Fatalf("unexpected MCP resource tool %q in tool-less turn; tools=%v", resTool, names)
			}
		}
	}
	for _, want := range wantTools {
		if !slices.Contains(names, want) {
			t.Fatalf("missing expected tool %q; tools=%v", want, names)
		}
	}
	sys := systemPromptText(req)
	if strings.Contains(sys, "You are Antigravity") || strings.Contains(sys, "<identity>") {
		t.Fatalf("default Antigravity system prompt leaked in clean mode: %q", sys)
	}
}

func TestRealACP(t *testing.T) {
	bin := os.Getenv("AGY_REAL_BINARY")
	if bin == "" {
		t.Skip("set AGY_REAL_BINARY inside a loopback-only Linux namespace")
	}
	if err := offlineNamespace(); err != nil {
		t.Fatal(err)
	}
	cc := &ccpaCapture{}
	up := httptest.NewServer(cc)
	defer up.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := defaults()
	c.cwd = t.TempDir()
	c.turnTimeout = 75 * time.Second
	home := t.TempDir()
	g := newGateway(ctx, c)
	cc.sentinel = filepath.Join(c.cwd, "permission-canary.txt")
	bridge := httptest.NewServer(http.HandlerFunc(g.mcp))
	defer bridge.Close()
	g.cfg.bridgeURL = bridge.URL
	cmd, err := offlineCommand(bin, up.URL, "", home, c.cwd)
	if err != nil {
		t.Fatal(err)
	}
	worker := make(chan error, 1)
	go func() { _, err := runProcess(ctx, cmd, g); worker <- err }()
	ended := false
	defer func() {
		cancel()
		if !ended {
			<-worker
		}

	}()
	deadline := time.After(40 * time.Second)
	for len(g.modelsList()) == 0 {
		select {
		case err := <-worker:
			ended = true
			t.Fatal(err)
		case <-deadline:
			t.Fatal("worker not ready")
		case <-time.After(50 * time.Millisecond):
		}
	}
	api := httptest.NewServer(g)
	defer api.Close()
	catalog := invoke(ctx, g, "GET", "/v1/models", "", nil)
	var models struct {
		Data []modelInfo `json:"data"`
	}
	if catalog.Code != 200 || json.Unmarshal(catalog.Body.Bytes(), &models) != nil || len(models.Data) == 0 {
		t.Fatal("native model catalog unavailable", catalog.Body.String())
	}
	if models.Data[0].Name == "" || models.Data[0].Name == models.Data[0].ID {
		t.Fatal("missing native model display name", models.Data)
	}
	post := func(body obj) obj {
		t.Helper()
		res, err := http.Post(api.URL+"/v1/responses", "application/json", strings.NewReader(enc(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("HTTP %d: %s", res.StatusCode, b)
		}
		var v obj
		if err = json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	// A deterministic 1x1 PNG; the CCPA capture verifies the inline image bytes.
	img := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl7p2QAAAAASUVORK5CYII="
	a := post(obj{"instructions": "offline-system-marker", "input": []obj{{"role": "user", "content": []obj{{"type": "input_text", "text": "first-offline"}, {"type": "input_image", "image_url": "data:image/png;base64," + img}}}}})
	if !strings.Contains(enc(a["output"]), "offline-final") {
		t.Fatal(a)
	}
	b := post(obj{"previous_response_id": a["id"], "instructions": "offline-system-marker", "input": "second-offline"})
	if !strings.Contains(enc(b["output"]), "offline-final") {
		t.Fatal(b)
	}
	cc.mu.Lock()
	turns := slices.Clone(cc.turns)
	cc.mu.Unlock()
	if len(turns) < 2 {
		t.Fatalf("only %d model requests", len(turns))
	}
	r1, r2 := turns[0], turns[len(turns)-1]
	wantR1Sys := effectiveSystemPrompt("offline-system-marker", false, true)
	if str(r1["sessionId"]) == "" || r1["sessionId"] != r2["sessionId"] || !strings.Contains(enc(r1["contents"]), img) || systemPromptText(r1) != wantR1Sys {
		t.Fatalf("multi-turn session, exact system prompt or image mapping mismatch: got=%q want=%q", systemPromptText(r1), wantR1Sys)
	}
	assertCleanTurn(t, r1, nil, false)
	if asObj(asObj(b["usage"])["input_tokens_details"])["cached_tokens"] != float64(3) {
		t.Fatalf("cached usage: %v", b["usage"])
	}
	// A clean turn without client instructions sends only the residual-tool soft-block guard.
	noSys := post(obj{"input": "no-system-probe"})
	if !strings.Contains(enc(noSys["output"]), "offline-final") {
		t.Fatal(noSys)
	}
	cc.mu.Lock()
	noSysTurn := cc.turns[len(cc.turns)-1]
	cc.mu.Unlock()
	if got := systemPromptText(noSysTurn); got != softBlockNoToolsPrompt {
		t.Fatalf("expected only no-tools soft-block prompt, got %q", got)
	}
	assertCleanTurn(t, noSysTurn, nil, false)
	t.Log("real ACP clean mode multi-turn, soft-block system prompt, image, tool stripping and cached usage passed")
	weather := obj{"type": "function", "name": "get_weather", "description": "Get weather for a city", "parameters": obj{"type": "object", "properties": obj{"city": obj{"type": "string"}}, "required": []string{"city"}}}
	first := post(obj{"input": "weather-probe", "tools": []obj{weather}})
	var call obj
	for _, raw := range arr(first["output"]) {
		o := asObj(raw)
		if o["type"] == "function_call" {
			call = o
			break
		}
	}
	if call == nil {
		cc.mu.Lock()
		names := slices.Clone(cc.names)
		cc.mu.Unlock()

		t.Fatalf("no MCP callback; declared tools: %v", names)
	}
	cc.mu.Lock()
	toolTurn := cc.turns[len(cc.turns)-1]
	cc.mu.Unlock()
	assertCleanTurn(t, toolTurn, []string{"mcp_vbridge_get_weather"}, true)
	if got := systemPromptText(toolTurn); got != softBlockWithToolsPrompt {
		t.Fatalf("expected with-tools soft-block prompt, got %q", got)
	}
	final := post(obj{"previous_response_id": first["id"], "input": []obj{{"type": "function_call_output", "call_id": call["call_id"], "output": []obj{{"type": "input_text", "text": "offline-weather-result"}, {"type": "input_image", "image_url": "data:image/png;base64," + img}}}}})
	cc.mu.Lock()
	last := enc(cc.turns[len(cc.turns)-1]["contents"])
	cname := cc.toolName
	cc.mu.Unlock()
	if !strings.Contains(last, "functionResponse") || !strings.Contains(last, "offline-weather-result") || !strings.Contains(last, img) || !strings.Contains(enc(final["output"]), "offline-final") {
		t.Fatal("tool result did not reach model and client")
	}
	t.Logf("real MCP closed loop using %s", cname)
	// The normal Chat endpoint must also resume an already running real ACP prompt.
	chatPost := func(body obj) obj {
		t.Helper()
		r, err := http.Post(api.URL+"/v1/chat/completions", "application/json", strings.NewReader(enc(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var v obj
		if err = json.NewDecoder(r.Body).Decode(&v); err != nil || r.StatusCode != 200 {
			t.Fatalf("chat HTTP %d %v %v", r.StatusCode, err, v)
		}
		return v
	}
	userMsg := user("weather-probe")
	q := obj{"model": "gemini-2.5-flash", "messages": []obj{userMsg}, "tools": chatTools("get_weather")}
	reply := chatPost(q)
	if choice(reply)["finish_reason"] != "tool_calls" {
		t.Fatal(reply)
	}
	a2 := assistant(reply)
	call2 := asObj(arr(a2["tool_calls"])[0])
	if asObj(call2["function"])["name"] != "get_weather" || asObj(call2["function"])["arguments"] != `{"city":"Shanghai"}` {
		t.Fatal(call2)
	}
	q["messages"] = []obj{userMsg, a2, {"role": "tool", "tool_call_id": call2["id"], "content": "offline-chat-result"}}
	if chatPostResult := chatPost(q); assistant(chatPostResult)["content"] != "offline-final" {
		t.Fatal(chatPostResult)
	}
	t.Log("real Chat tool result and original argument schema passed")
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		body := obj{"input": "stream offline", "stream": true}
		if path == "/v1/chat/completions" {
			body = obj{"messages": []obj{user("stream offline")}, "stream": true, "stream_options": obj{"include_usage": true}}
		}
		r, err := http.Post(api.URL+path, "application/json", strings.NewReader(enc(body)))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil || r.StatusCode != 200 || !strings.Contains(string(data), "offline-final") {
			t.Fatalf("real stream %s: %v %s", path, err, data)
		}
		if path == "/v1/responses" && !strings.Contains(string(data), "event: response.completed") || path == "/v1/chat/completions" && !strings.Contains(string(data), "data: [DONE]") {
			t.Fatalf("missing terminal event for %s", path)
		}
	}

	// Defense-in-depth: when explicitly running agy's built-in default agent (or on fallback),
	// built-in write_file and run_command calls must still be denied by session/request_permission.
	t.Run("default_agent_permission_denial", func(t *testing.T) {
		cc2 := &ccpaCapture{}
		up2 := httptest.NewServer(cc2)
		defer up2.Close()
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		c2 := defaults()
		c2.cwd = t.TempDir()
		c2.turnTimeout = 75 * time.Second
		home2 := t.TempDir()
		g2 := newGateway(ctx2, c2)
		cc2.sentinel = filepath.Join(c2.cwd, "permission-canary.txt")
		bridge2 := httptest.NewServer(http.HandlerFunc(g2.mcp))
		defer bridge2.Close()
		g2.cfg.bridgeURL = bridge2.URL
		cmd2, err := offlineCommand(bin, up2.URL, "default", home2, c2.cwd)
		if err != nil {
			t.Fatal(err)
		}
		w2 := make(chan error, 1)
		go func() { _, err := runProcess(ctx2, cmd2, g2); w2 <- err }()
		ended2 := false
		defer func() {
			cancel2()
			if !ended2 {
				<-w2
			}
		}()
		dl := time.After(40 * time.Second)
		for len(g2.modelsList()) == 0 {
			select {
			case err := <-w2:
				ended2 = true
				t.Fatal(err)
			case <-dl:
				t.Fatal("default agent worker not ready")
			case <-time.After(50 * time.Millisecond):
			}
		}
		api2 := httptest.NewServer(g2)
		defer api2.Close()
		post2 := func(body obj) {
			t.Helper()
			res, err := http.Post(api2.URL+"/v1/responses", "application/json", strings.NewReader(enc(body)))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != 200 {
				b, _ := io.ReadAll(res.Body)
				t.Fatalf("HTTP %d: %s", res.StatusCode, b)
			}
		}
		post2(obj{"input": "write-canary"})
		if _, err := os.Stat(cc2.sentinel); !os.IsNotExist(err) {
			t.Fatal("builtin wrote the canary")
		}
		if g2.denied.Load() == 0 {
			t.Fatal("expected a denied native write permission request")
		}
		before := g2.denied.Load()
		post2(obj{"input": "command-canary"})
		if _, err := os.Stat(cc2.sentinel); !os.IsNotExist(err) {
			t.Fatal("builtin command created the canary")
		}
		if g2.denied.Load() <= before {
			t.Fatal("expected a denied native command permission request")
		}
		t.Log("real ACP default-agent builtin write and command denied; canary absent")
	})

}

// The two subtests have separate gateway processes, worker processes, and homes.
// Only the request history and the loopback CCPA observer survive the restart.
func TestRealResponsesStatelessRestart(t *testing.T) {
	bin, agy := os.Getenv("AGY_GATEWAY_BINARY"), os.Getenv("AGY_REAL_BINARY")
	if bin == "" || agy == "" {
		t.Skip("requires built gateway and offline agy fixture")
	}
	if err := offlineNamespace(); err != nil {
		t.Fatal(err)
	}
	cc := &ccpaCapture{}
	up := httptest.NewServer(cc)
	defer up.Close()
	args := []string{"-agy", agy, "-mock-ccpa", up.URL}
	history := []any{user("stateless-weather-probe")}
	tools := specs("get_weather")
	tools[0]["parameters"] = obj{"type": "object", "properties": obj{"city": obj{"type": "string"}}, "required": []string{"city"}}
	post := func(t *testing.T, client *http.Client, addr string, body obj, want int) obj {
		t.Helper()
		res, err := client.Post("http://"+addr+"/v1/responses", "application/json", strings.NewReader(enc(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var v obj
		if err = json.NewDecoder(res.Body).Decode(&v); err != nil || res.StatusCode != want {
			t.Fatalf("HTTP %d: %v %v", res.StatusCode, err, v)
		}
		return v
	}
	var original, call obj
	if !t.Run("issue", func(t *testing.T) {
		addr, _, client := startTestCLI(t, bin, args...)
		original = post(t, client, addr, obj{"store": false, "input": history, "tools": tools, "instructions": "first-process-only"}, 200)
		calls := responseCalls(original)
		if len(calls) != 1 || calls[0]["name"] != "get_weather" {
			t.Fatal(original)
		}
		call = calls[0]
		post(t, client, addr, obj{"previous_response_id": original["id"], "input": "check"}, 400)
	}) {
		t.FailNow()
	}
	image := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl7p2QAAAAASUVORK5CYII="
	value := uid("replay-value-")
	if !t.Run("resume_after_shutdown", func(t *testing.T) {
		addr, _, client := startTestCLI(t, bin, args...)
		result := callResult(call["call_id"], []obj{{"type": "input_text", "text": value}, {"type": "input_image", "image_url": "data:image/png;base64," + image}})
		post(t, client, addr, obj{"previous_response_id": original["id"], "input": []any{result}}, 400)
		post(t, client, addr, obj{"store": false, "input": []any{result}}, 400)
		next := post(t, client, addr, obj{"store": false, "input": withOutput(history, original, result), "tools": tools}, 200)
		if responseText(next) != "replayed "+value || next["store"] != false || len(responseCalls(next)) != 0 {
			t.Fatal(next)
		}
		// Branch from the same returned call. This time the mock model needs
		// another tool; there is no user message between the two results.
		branch := withOutput(history, original, callResult(call["call_id"], "followup-tool-probe"))
		another := post(t, client, addr, obj{"store": false, "input": branch, "tools": tools}, 200)
		calls := responseCalls(another)
		if len(calls) != 1 || calls[0]["call_id"] == call["call_id"] || calls[0]["arguments"] != `{"city":"Suzhou"}` {
			t.Fatal(another)
		}
		value2 := uid("replay-value-")
		last := post(t, client, addr, obj{"store": false, "input": withOutput(branch, another, callResult(calls[0]["call_id"], value2))}, 200)
		if responseText(last) != "replayed "+value2 {
			t.Fatal(last)
		}
		// Chat Completions tool replay across restart: submit the prior call & result with no live pending session.
		chatVal := uid("replay-value-")
		chatReq := obj{
			"model": "gemini-2.5-flash",
			"messages": []obj{
				user("stateless-weather-probe"),
				{"role": "assistant", "content": nil, "tool_calls": []obj{historyCall(str(call["call_id"]), "get_weather", str(call["arguments"]))}},
				{"role": "tool", "tool_call_id": str(call["call_id"]), "content": chatVal},
			},
			"tools": chatTools("get_weather"),
		}
		resChat, err := client.Post("http://"+addr+"/v1/chat/completions", "application/json", strings.NewReader(enc(chatReq)))
		if err != nil {
			t.Fatal(err)
		}
		var chatOut obj
		err = json.NewDecoder(resChat.Body).Decode(&chatOut)
		resChat.Body.Close()
		if err != nil || resChat.StatusCode != 200 || assistant(chatOut)["content"] != "replayed "+chatVal {
			t.Fatalf("chat replay across restart HTTP %d: %v %v", resChat.StatusCode, err, chatOut)
		}

	}) {
		t.FailNow()
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	var first, last obj
	for _, req := range cc.turns {
		input := enc(req["contents"])
		if strings.Contains(input, value) {
			last = req
		} else if first == nil && strings.Contains(input, "stateless-weather-probe") {
			first = req
		}
	}
	if first == nil || last == nil || str(first["sessionId"]) == "" || str(last["sessionId"]) == "" || first["sessionId"] == last["sessionId"] {
		t.Fatal("expected different model session IDs across restart")
	}
	input := enc(last["contents"])
	for _, marker := range []string{str(call["call_id"]), value, image, "stateless-weather-probe", "function_call_output", "Shanghai"} {
		if !strings.Contains(input, marker) {
			t.Fatalf("missing replayed content %q", marker)
		}
	}
	if strings.Contains(input, "functionResponse") || strings.Contains(enc(last["systemInstruction"]), "first-process-only") {
		t.Fatal("unexpected native callback or inherited instructions")
	}
	assertCleanTurn(t, first, []string{"mcp_vbridge_get_weather"}, true)
	assertCleanTurn(t, last, []string{"mcp_vbridge_get_weather"}, true)
	t.Log("new gateway and real agy read the original call, returned result and inline image from client history")
}
