package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponsesTranscriptReuse(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		name := "stored"
		if stateless {
			name = "stateless with cache"
		}
		t.Run(name, func(t *testing.T) {
			c := defaults()
			c.cacheStateless = stateless
			f := fixtureFor(t, c)
			h := []any{user("tools: weather and clock")}
			tools := specs("weather", "clock")
			first := postJSON(t, f.g, "responses", obj{"input": h, "tools": tools, "store": !stateless}, 200)
			calls := responseCalls(first)
			if len(calls) != 2 {
				t.Fatal(first)
			}
			input := withOutput(h, first, callResult(calls[1]["call_id"], "cloudy"), callResult(calls[0]["call_id"], "18:00"))
			for i, raw := range input {
				if item := asObj(raw); item["type"] == "function_call" {
					clone := obj{}
					for k, v := range item {
						clone[k] = v
					}
					clone["arguments"] = "  " + str(item["arguments"]) + "  "
					input[i] = clone
				}
			}
			next := postJSON(t, f.g, "responses", obj{"input": input, "tools": tools, "store": !stateless}, 200)
			if !strings.Contains(responseText(next), "cloudy") || len(f.framesFor("session/prompt")) != 1 {
				t.Fatal("did not resume the original tool calls", next)
			}
			last := postJSON(t, f.g, "responses", obj{"input": withOutput(input, next, user("next turn")), "tools": tools, "store": !stateless}, 200)
			if responseText(last) != "echo: next turn" || len(f.framesFor("session/new")) != 1 {
				t.Fatal("did not reuse the ACP session", last, f.framesFor("session/new"))
			}
			stats := f.g.telemetry.dashboard()
			if stats["session_reused"] != uint64(1) || stats["session_rebuilt"] != uint64(1) || stats["tool_resumed"] != uint64(1) {
				t.Fatal(stats)
			}
			postJSON(t, f.g, "responses", obj{"previous_response_id": first["id"], "input": "obsolete"}, 400)
			if stateless {
				postJSON(t, f.g, "responses", obj{"previous_response_id": last["id"], "input": "private"}, 400)
			}
		})
	}
}

func TestChatConcurrentTranscriptBranches(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		f := fixtureFor(t, defaults())
		request := func(messages ...obj) obj {
			q := chat(messages...)
			q["user"] = "same-account"
			if explicit {
				q["prompt_cache_key"] = "same-explicit-key"
			}
			return q
		}
		first := user("same first turn")
		base := assistant(postJSON(t, f.g, "chat/completions", request(first), 200))
		branches := []obj{user("left"), user("right")}
		var answers []obj
		for _, branch := range branches {
			answers = append(answers, assistant(postJSON(t, f.g, "chat/completions", request(first, base, branch), 200)))
		}
		for i, branch := range branches {
			postJSON(t, f.g, "chat/completions", request(first, base, branch, answers[i], user("continue")), 200)
		}
		prompts := f.framesFor("session/prompt")
		if len(f.framesFor("session/new")) != 2 || prompts[1].Params["sessionId"] != prompts[3].Params["sessionId"] ||
			prompts[2].Params["sessionId"] != prompts[4].Params["sessionId"] {
			t.Fatal("conversation branches were not independently reused")
		}
	}
}

func TestModelDirectoryAndTelemetry(t *testing.T) {
	c := defaults()
	c.key, c.metrics = "fixture", true
	c.models = map[string]string{"fast": "mock", "obsolete": "missing"}
	c.usageFile = filepath.Join(t.TempDir(), "usage.json")
	g := fixtureFor(t, c).g
	g.modelDetails = map[string]modelInfo{"mock": {Name: "Native display name", ReasoningEffort: []string{"low", "high"}}}
	h := http.Header{"Authorization": {"Bearer fixture"}}
	get := func(path string, headers http.Header, status int) string {
		t.Helper()
		res := invoke(context.Background(), g, "GET", path, "", headers)
		if res.Code != status {
			t.Fatalf("%s: HTTP %d, want %d", path, res.Code, status)
		}
		return res.Body.String()
	}
	get("/v1/models/fast", nil, 401)
	var m modelInfo
	if err := json.Unmarshal([]byte(get("/v1/models/fast", h, 200)), &m); err != nil ||
		m.ID != "fast" || m.Name != "Native display name" || m.CanonicalID != "mock" || len(m.ReasoningEffort) != 2 {
		t.Fatal(m, err)
	}
	get("/v1/models/obsolete", h, 404)
	get("/metrics", nil, 401)
	secret := "private-transcript-marker"
	w := invoke(context.Background(), g, "POST", "/v1/responses", enc(obj{"model": "fast", "input": secret}), h)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	retry := int64(7)
	for i := 0; i < 22; i++ {
		f := problem(429, "upstream_rate_limited", secret)
		f.upstream, f.retryAfter = obj{"provider_http_status": 429}, &retry
		g.recordRequest(time.Now(), "/v1/responses", "req_fixture", m.CanonicalID, f, nil, lease{})
	}
	if err := g.saveUsage(); err != nil {
		t.Fatal(err)
	}
	reloaded := newGateway(context.Background(), c)
	if err := reloaded.loadUsage(); err != nil {
		t.Fatal(err)
	}
	if reloaded.usage.total.Load() != 23 || reloaded.usage.failed.Load() != 22 {
		t.Fatal(reloaded.usage.snapshot())
	}
	if data, err := os.ReadFile(c.usageFile); err != nil || strings.Contains(string(data), secret) || strings.Contains(string(data), "req_fixture") {
		t.Fatal("unexpected data in persisted usage")
	}
	summary := get("/admin/api/status", h, 200)
	if strings.Contains(summary, secret) || strings.Count(summary, `"request_id":"req_fixture"`) != 20 ||
		!strings.Contains(summary, `"cooldown_until"`) {
		t.Fatal(summary)
	}
	metrics := get("/metrics", h, 200)
	if !strings.Contains(metrics, "agy_gateway_requests_total 23\n") || !strings.Contains(metrics, "agy_gateway_request_duration_seconds_count 23\n") {
		t.Fatal(metrics)
	}
	h.Set("Content-Type", "application/json")
	res := invoke(context.Background(), g, "POST", "/admin/api/usage/reset", "{}", h)
	if res.Code != 200 || reloaded.loadUsage() != nil || reloaded.usage.total.Load() != 0 {
		t.Fatal(res.Code, reloaded.usage.snapshot())
	}
}
