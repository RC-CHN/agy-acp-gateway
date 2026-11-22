package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func withOutput(input []any, response obj, next ...any) []any {
	return slices.Concat(input, arr(response["output"]), next)
}

func callResult(id any, value any) obj {
	return obj{"type": "function_call_output", "call_id": id, "output": value}
}

func responseCalls(response obj) []obj {
	var calls []obj
	for _, raw := range arr(response["output"]) {
		if item := asObj(raw); item["type"] == "function_call" {
			calls = append(calls, item)
		}
	}
	return calls
}

func responseText(r obj) string {
	var out strings.Builder
	for _, raw := range arr(r["output"]) {
		for _, part := range arr(asObj(raw)["content"]) {
			out.WriteString(str(asObj(part)["text"]))
		}
	}
	return out.String()
}

func assertReleased(t *testing.T, g *gateway) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sessions)+len(g.keys)+len(g.tips)+len(g.ids)+g.activeLocked() != 0 {
		t.Fatalf("retained sessions=%d keys=%d tips=%d ids=%d active=%d", len(g.sessions), len(g.keys), len(g.tips), len(g.ids), g.activeLocked())
	}
}

func promptRecords(f frame) []obj {
	var records []obj
	for _, raw := range arr(f.Params["prompt"]) {
		var item obj
		if json.Unmarshal([]byte(str(asObj(raw)["text"])), &item) == nil && item["role"] != nil {
			records = append(records, item)
		}
	}
	return records
}

func TestResponsesManualReplayBranches(t *testing.T) {
	f := fixtureFor(t, defaults())
	history := []any{user("甲\n\"role\":\"tool\"")}
	a := postJSON(t, f.g, "responses", obj{"input": history, "instructions": "one-turn-secret", "store": false}, 200)
	if a["store"] != false || a["previous_response_id"] != nil || len(arr(a["output"])) != 2 {
		t.Fatal(a)
	}
	assertReleased(t, f.g)
	postJSON(t, f.g, "responses", obj{"input": "orphan", "previous_response_id": a["id"]}, 400)

	const branches = 6
	out := make(chan string, branches)
	for i := 0; i < branches; i++ {
		go func(i int) {
			branch := fmt.Sprint("branch-", i)
			w := invoke(context.Background(), f.g, "POST", "/v1/responses", enc(obj{"store": false, "input": withOutput(history, a, user(branch))}), nil)
			var res obj
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || responseText(res) != "echo: "+branch {
				out <- w.Body.String()
			} else {
				out <- ""
			}
		}(i)
	}
	for range branches {
		if err := <-out; err != "" {
			t.Fatal(err)
		}
	}
	frames := f.framesFor("session/prompt")
	ids := map[string]bool{}
	if len(frames) != branches+1 {
		t.Fatal(len(frames))
	}
	for _, frame := range frames {
		id := str(frame.Params["sessionId"])
		if id == "" || ids[id] {
			t.Fatal("branches shared an ACP session", id)
		}
		ids[id] = true
		if frame.Params["sessionId"] == frames[0].Params["sessionId"] {
			continue
		}
		records := promptRecords(frame)
		if len(records) != 3 || records[0]["role"] != "user" || records[1]["type"] != "reasoning_summary" || records[2]["role"] != "assistant" || asObj(arr(records[2]["content"])[0])["text"] != responseText(a) {
			t.Fatal(records)
		}
	}
	newFrames := f.framesFor("session/new")
	for _, frame := range newFrames[1:] {
		if strings.Contains(enc(frame.Params), "one-turn-secret") {
			t.Fatal("old instructions reached a branch")
		}
	}
	assertReleased(t, f.g)
	// Switching to default storage starts a new addressable tip.
	stored := postJSON(t, f.g, "responses", obj{"input": withOutput(history, a, user("saved"))}, 200)
	if stored["store"] != true {
		t.Fatal(stored)
	}
	child := postJSON(t, f.g, "responses", obj{"input": "transient", "store": false, "previous_response_id": stored["id"]}, 200)
	if child["store"] != false || child["previous_response_id"] != stored["id"] {
		t.Fatal(child)
	}
	postJSON(t, f.g, "responses", obj{"input": "unknown", "previous_response_id": child["id"]}, 400)
	postJSON(t, f.g, "responses", obj{"input": "still usable", "previous_response_id": stored["id"]}, 200)
}

func TestResponsesManualParallelToolReplay(t *testing.T) {
	c := defaults()
	c.maxSessions = 1
	c.maxTurns = 1
	f := fixtureFor(t, c)
	h := []any{user("tools: 并行")}
	v := postJSON(t, f.g, "responses", obj{"store": false, "input": h, "tools": specs("weather", "clock")}, 200)
	calls := responseCalls(v)
	if len(calls) != 2 {
		t.Fatal(v)
	}
	assertReleased(t, f.g)
	results := []any{callResult(calls[1]["call_id"], "结果B"), callResult(calls[0]["call_id"], "结果A")}
	h = withOutput(h, v, results...)
	for i := 0; i < 2; i++ {
		res := postJSON(t, f.g, "responses", obj{"store": false, "input": h}, 200)
		if !strings.Contains(responseText(res), "结果A") || !strings.Contains(responseText(res), "结果B") || len(responseCalls(res)) != 0 {
			t.Fatal(res)
		}
		assertReleased(t, f.g)
	}
	// Start a second round using the complete first round and a new tool set.
	h = append(slices.Clone(h), user("tools: again"))
	next := postJSON(t, f.g, "responses", obj{"store": false, "input": h, "tools": specs("second"), "model": "mock-alt"}, 200)
	if len(responseCalls(next)) != 1 {
		t.Fatal(next)
	}
	last := postJSON(t, f.g, "responses", obj{"store": false, "input": withOutput(h, next, callResult(responseCalls(next)[0]["call_id"], "42"))}, 200)
	if !strings.Contains(responseText(last), "42") || len(f.framesFor("session/prompt")) != 5 {
		t.Fatal(last)
	}
	assertReleased(t, f.g)
}

func TestResponsesManualResultCannotConsumeStoredTurn(t *testing.T) {
	f := fixtureFor(t, defaults())
	h := []any{user("tools: weather")}
	first := postJSON(t, f.g, "responses", obj{"input": h, "tools": specs("weather")}, 200)
	id := responseCalls(first)[0]["call_id"]
	result := callResult(id, "cold")
	postJSON(t, f.g, "responses", obj{"input": []any{result}}, 400)
	postJSON(t, f.g, "responses", obj{"previous_response_id": first["id"], "input": "premature"}, 400)
	for _, req := range []obj{
		{"input": withOutput(h, first, result), "tools": specs("weather")},
		{"input": []any{result}, "previous_response_id": first["id"], "store": false},
	} {
		r := postJSON(t, f.g, "responses", req, 200)
		if !strings.Contains(responseText(r), "cold") {
			t.Fatal(r)
		}
	}
	native := postJSON(t, f.g, "responses", obj{"previous_response_id": first["id"], "input": []any{callResult(id, "native")}}, 200)
	if !strings.Contains(responseText(native), "native") || len(f.framesFor("session/prompt")) != 3 {
		t.Fatal(native)
	}
}

func TestResponsesReplayValidation(t *testing.T) {
	f := fixtureFor(t, defaults())
	call := obj{"type": "function_call", "name": "weather", "call_id": "a", "arguments": "{}"}
	second := obj{"type": "function_call", "name": "clock", "call_id": "b", "arguments": "{}"}
	ok := callResult("a", "12")
	summary := obj{"type": "reasoning", "summary": []obj{{"type": "summary_text", "text": "摘要"}}}
	for name, input := range map[string]any{
		"orphan": []any{ok}, "duplicate output": []any{call, ok, ok}, "duplicate call": []any{call, ok, call, ok},
		"partial parallel batch": []any{call, second, ok}, "unanswered before user": []any{call, user("next"), ok},
		"bad old round": []any{ok, user("next")}, "result precedes call": []any{ok, call},
		"wrong id": []any{call, callResult("unknown", "x")}, "malformed ciphertext": []any{obj{"type": "reasoning", "summary": []any{}, "encrypted_content": obj{}}},
		"opaque reasoning":        []any{obj{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque"}, user("x")},
		"bad summary":             []any{obj{"type": "reasoning", "summary": []obj{{"type": "output_text", "text": "x"}}}, user("x")},
		"missing summary":         []any{obj{"type": "reasoning"}, user("x")},
		"item reference":          []any{obj{"type": "item_reference", "id": "msg_1"}, user("x")},
		"object arguments":        []any{obj{"type": "function_call", "name": "weather", "call_id": "a", "arguments": obj{}}, ok},
		"wrong call id attribute": []any{call, obj{"type": "function_call_output", "tool_call_id": "a", "output": "x"}},
		"wrong result type":       []any{call, callResult("a", obj{"value": 1})},
		"chat tool shape":         []any{obj{"role": "assistant", "tool_calls": []any{}, "content": "x"}, user("x")},
	} {
		t.Run(name, func(t *testing.T) { postJSON(t, f.g, "responses", obj{"store": false, "input": input}, 400) })
	}
	for _, b := range []obj{{"input": "x", "store": "false"}, {"input": "x", "store": 0}, {"input": "x", "conversation": "conv_unknown"}, {"input": "x", "previous_response_id": 42}} {
		postJSON(t, f.g, "responses", b, 400)
	}
	if len(f.framesFor("session/new")) != 0 {
		t.Fatal("invalid history opened a session")
	}
	// Keep argument strings verbatim and image bytes in native ACP image blocks.
	args := " { \"n\" : 9007199254740993123456789 } "
	call["arguments"] = args
	summary["encrypted_content"] = "do-not-inject-this"
	input := []any{obj{"type": "reasoning", "summary": []any{}}, summary, call, callResult("a", []obj{
		{"type": "input_text", "text": "first\nline"}, {"type": "input_image", "image_url": "data:image/png;base64,AQID"}, {"type": "input_text", "text": "after"}})}
	postJSON(t, f.g, "responses", obj{"input": input, "store": false}, 200)
	frame := f.framesFor("session/prompt")[0]
	records := promptRecords(frame)
	if len(records) != 3 || str(asObj(asObj(arr(records[1]["tool_calls"])[0])["function"])["arguments"]) != args || strings.Contains(enc(frame.Params), "do-not-inject-this") {
		t.Fatal(records)
	}
	parts := arr(records[2]["content"])
	prompt := arr(frame.Params["prompt"])
	if len(parts) != 3 || asObj(parts[1])["following_image"] != float64(1) || asObj(parts[2])["text"] != "after" || asObj(prompt[len(prompt)-2])["data"] != "AQID" {
		t.Fatal(prompt)
	}
}

func TestResponsesStatelessSSETerminals(t *testing.T) {
	f := fixtureFor(t, defaults())
	for _, prompt := range []string{"hello", "length: truncated", "tools: lookup"} {
		h := []any{user(prompt)}
		w := invoke(context.Background(), f.g, "POST", "/v1/responses", enc(obj{"store": false, "stream": true, "input": h, "tools": specs("lookup")}), nil)
		es := events(t, w.Body.Bytes())
		if len(es) < 3 {
			t.Fatal(w.Body.String())
		}
		terminal := es[len(es)-1]
		want := "response.completed"
		if strings.HasPrefix(prompt, "length:") {
			want = "response.incomplete"
		}
		first, last := asObj(es[0]["response"]), asObj(terminal["response"])
		if terminal["type"] != want || last["store"] != false || first["store"] != false || last["id"] != first["id"] {
			t.Fatal(terminal)
		}
		postJSON(t, f.g, "responses", obj{"input": "by ID", "previous_response_id": last["id"]}, 400)
		var next any = user("after SSE")
		if calls := responseCalls(last); len(calls) > 0 {
			next = callResult(calls[0]["call_id"], "stream result")
		}
		postJSON(t, f.g, "responses", obj{"input": withOutput(h, last, next), "store": false}, 200)
		assertReleased(t, f.g)
	}
}

func TestResponsesStatelessCapacityAndCancellation(t *testing.T) {
	c := defaults()
	c.maxTurns = 1
	c.maxSessions = 1
	f := fixtureFor(t, c)
	stored := postJSON(t, f.g, "responses", obj{"input": "saved"}, 200)
	postJSON(t, f.g, "responses", obj{"previous_response_id": stored["id"], "input": "temporary", "store": false}, 429)
	postJSON(t, f.g, "responses", obj{"previous_response_id": stored["id"], "input": "still valid"}, 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		invoke(ctx, f.g, "POST", "/v1/responses", enc(obj{"input": "wait: cancel", "store": false, "stream": true}), nil)
		close(done)
	}()
	until(t, func() bool { return len(f.framesFor("session/prompt")) == 3 })
	postJSON(t, f.g, "responses", obj{"input": "full", "store": false}, 429)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stuck request")
	}
	assertReleased(t, f.g)
	postJSON(t, f.g, "responses", obj{"input": "recovered", "store": false}, 200)
}

func TestResponsesExpiredStatefulToolReplayed(t *testing.T) {
	c := defaults()
	c.batch = 2 * time.Millisecond
	c.toolTimeout = 80 * time.Millisecond
	f := fixtureFor(t, c)
	h := []any{user("tools: once")}
	r := postJSON(t, f.g, "responses", obj{"input": h, "tools": specs("once")}, 200)
	call := responseCalls(r)[0]
	result := callResult(call["call_id"], "after expiry")
	until(t, func() bool { f.g.mu.Lock(); defer f.g.mu.Unlock(); return len(f.g.sessions) == 0 })
	postJSON(t, f.g, "responses", obj{"previous_response_id": r["id"], "input": []any{result}}, 400)
	res := postJSON(t, f.g, "responses", obj{"input": withOutput(h, r, result), "store": false, "tools": specs("other"), "model": "mock-alt", "reasoning": obj{"effort": "high"}}, 200)
	if !strings.Contains(responseText(res), "after expiry") || len(f.framesFor("session/set_model")) != 1 || len(f.framesFor("session/set_config_option")) != 1 {
		t.Fatal(res)
	}
	assertReleased(t, f.g)
}

func TestResponsesReplayContentOrder(t *testing.T) {
	f := fixtureFor(t, defaults())
	h := []any{
		obj{"role": "developer", "content": "apply-this-system"},
		user("start"), obj{"type": "reasoning", "summary": []obj{{"type": "summary_text", "text": "R1"}, {"type": "summary_text", "text": "R2"}}},
		obj{"type": "message", "role": "assistant", "content": []obj{{"type": "output_text", "text": "A1"}}},
		obj{"type": "reasoning", "summary": []obj{{"type": "summary_text", "text": "R3"}}},
	}
	postJSON(t, f.g, "responses", obj{"input": h, "store": false}, 200)
	first := f.framesFor("session/prompt")[0]
	r := promptRecords(first)
	if len(r) != 4 || asObj(arr(r[1]["content"])[1])["text"] != "R2" || r[2]["role"] != "assistant" || asObj(arr(r[3]["content"])[0])["text"] != "R3" {
		t.Fatal(r)
	}
	if strings.Contains(enc(first.Params), "apply-this-system") || !strings.Contains(enc(f.framesFor("session/new")[0].Params["_meta"]), "apply-this-system") {
		t.Fatal("developer instruction misplaced")
	}
	h = append(h, obj{"role": "assistant", "content": "assistant tail"})
	postJSON(t, f.g, "responses", obj{"input": h, "store": false}, 200)
	r = promptRecords(f.framesFor("session/prompt")[1])
	if r[len(r)-1]["role"] != "assistant" {
		t.Fatal(r)
	}
}

func TestChatToolReplayFallback(t *testing.T) {
	// 1. Cold/restarted gateway replays a complete Chat Completions tool transcript and
	// seamlessly resumes linear stateful session reuse on subsequent user turns.
	f1 := fixtureFor(t, defaults())
	q1 := chat(user("tools: weather and clock"))
	q1["tools"] = chatTools("weather", "clock")
	first := postJSON(t, f1.g, "chat/completions", q1, 200)
	a1 := assistant(first)
	calls := arr(a1["tool_calls"])
	if len(calls) != 2 {
		t.Fatal(first)
	}
	toolResults := []obj{
		{"role": "tool", "tool_call_id": str(asObj(calls[1])["id"]), "content": "12:00"},
		{"role": "tool", "tool_call_id": str(asObj(calls[0])["id"]), "content": "Sunny 20C"},
	}

	f2 := fixtureFor(t, defaults())
	// Validation must still reject incomplete, duplicate, or orphan tool transcripts on a cold gateway.
	postJSON(t, f2.g, "chat/completions", chat(user("hi"), toolResults[0]), 400)
	postJSON(t, f2.g, "chat/completions", chat(user("tools: weather and clock"), a1, toolResults[0]), 400)
	postJSON(t, f2.g, "chat/completions", chat(user("tools: weather and clock"), a1, toolResults[0], toolResults[0]), 400)
	postJSON(t, f2.g, "chat/completions", chat(user("hi"), obj{"role": "assistant", "content": "done"}), 400)

	msgs := []obj{user("tools: weather and clock"), a1, toolResults[0], toolResults[1]}
	q2 := chat(msgs...)
	q2["tools"] = q1["tools"]
	replayed := postJSON(t, f2.g, "chat/completions", q2, 200)
	a2 := assistant(replayed)
	if !strings.Contains(str(a2["content"]), "Sunny 20C") || !strings.Contains(str(a2["content"]), "12:00") {
		t.Fatal(replayed)
	}
	if len(f2.framesFor("session/new")) != 1 || !strings.Contains(enc(f2.framesFor("session/prompt")[0].Params["prompt"]), continueReplay) {
		t.Fatal("expected single replay session with continueReplay prompt")
	}
	// Next user turn should reuse the newly built stateful session without another session/new.
	q3 := chat(append(slices.Clone(msgs), a2, user("followup"))...)
	q3["tools"] = q1["tools"]
	next := postJSON(t, f2.g, "chat/completions", q3, 200)
	if assistant(next)["content"] != "echo: followup" || len(f2.framesFor("session/new")) != 1 {
		t.Fatal(next)
	}

	// 2. Expired pending tool turn falls back to replay cleanly once the client submits results.
	c := defaults()
	c.batch = 2 * time.Millisecond
	c.toolTimeout = 80 * time.Millisecond
	fExp := fixtureFor(t, c)
	qExp := chat(user("tools: once"))
	qExp["tools"] = chatTools("once")
	rExp := postJSON(t, fExp.g, "chat/completions", qExp, 200)
	aExp := assistant(rExp)
	callID := str(asObj(arr(aExp["tool_calls"])[0])["id"])
	until(t, func() bool { fExp.g.mu.Lock(); defer fExp.g.mu.Unlock(); return len(fExp.g.sessions) == 0 })
	qAfter := chat(user("tools: once"), aExp, obj{"role": "tool", "tool_call_id": callID, "content": "after-chat-expiry"})
	qAfter["tools"] = chatTools("other")
	resAfter := postJSON(t, fExp.g, "chat/completions", qAfter, 200)
	if !strings.Contains(str(assistant(resAfter)["content"]), "after-chat-expiry") {
		t.Fatal(resAfter)
	}

	// 3. Changing tools/system on the same explicit session while returning tool results cancels the old pending turn and replays cleanly.
	fEdit := fixtureFor(t, defaults())
	qEdit := chat(user("tools: once"))
	qEdit["tools"] = chatTools("once")
	qEdit["prompt_cache_key"] = "edit-session"
	rEdit := postJSON(t, fEdit.g, "chat/completions", qEdit, 200)
	aEdit := assistant(rEdit)
	editID := str(asObj(arr(aEdit["tool_calls"])[0])["id"])
	qEdit2 := chat(obj{"role": "system", "content": "new-sys"}, user("tools: once"), aEdit, obj{"role": "tool", "tool_call_id": editID, "content": "edited-tool-res"})
	qEdit2["tools"] = chatTools("once")
	qEdit2["prompt_cache_key"] = "edit-session"
	resEdit := postJSON(t, fEdit.g, "chat/completions", qEdit2, 200)
	if !strings.Contains(str(assistant(resEdit)["content"]), "edited-tool-res") || len(fEdit.framesFor("session/new")) != 2 {
		t.Fatal(resEdit)
	}
	until(t, func() bool { return len(fEdit.framesFor("session/cancel")) >= 1 })
}

func TestResponsesReplayBodyLimit(t *testing.T) {
	c := defaults()
	c.bodyLimit = 512
	f := fixtureFor(t, c)
	h := []any{user(strings.Repeat("x", 200))}
	a := postJSON(t, f.g, "responses", obj{"input": h, "store": false}, 200)
	postJSON(t, f.g, "responses", obj{"input": withOutput(h, a, user("next")), "store": false}, 413)
	assertReleased(t, f.g)
	if len(f.framesFor("session/prompt")) != 1 {
		t.Fatal("oversized replay reached worker")
	}
}
