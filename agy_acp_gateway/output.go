package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type output struct {
	w             http.ResponseWriter
	q             request
	id            string
	created       int64
	seq           int
	items         []obj
	active        string
	part          strings.Builder
	text, thought strings.Builder
	calls         []obj
	streamErr     error
}

func newOutput(w http.ResponseWriter, q request) *output {
	p := "chatcmpl-"
	if q.responses {
		p = "resp_"
	}
	return &output{w: w, q: q, id: uid(p), created: time.Now().Unix(), items: []obj{}}
}
func (o *output) send(kind string, v obj) {
	if !o.q.stream || o.streamErr != nil {
		return
	}
	_ = http.NewResponseController(o.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if kind != "" {
		v["type"] = kind
		v["sequence_number"] = o.seq
		o.seq++
		_, o.streamErr = fmt.Fprintf(o.w, "event: %s\n", kind)
	}
	if o.streamErr == nil {
		b, err := json.Marshal(v)
		if err == nil {
			_, err = fmt.Fprintf(o.w, "data: %s\n\n", b)
		}
		o.streamErr = err
	}
	if o.streamErr == nil {
		o.streamErr = http.NewResponseController(o.w).Flush()
	}
}
func (o *output) chat(object string, choices []obj) obj {
	return obj{"id": o.id, "created": o.created, "model": o.q.model, "object": object, "choices": choices}
}
func (o *output) chunk(delta obj, finish any) {
	o.send("", o.chat("chat.completion.chunk", []obj{{"index": 0, "delta": delta, "finish_reason": finish}}))
}
func (o *output) response(status string, usage any) obj {
	var prev any
	if o.q.previous != "" {
		prev = o.q.previous
	}
	return obj{"store": o.q.store, "previous_response_id": prev, "id": o.id, "object": "response", "created_at": o.created, "model": o.q.model, "status": status, "output": o.items, "usage": usage, "error": nil, "incomplete_details": nil}
}
func (o *output) start() {
	if !o.q.stream {
		return
	}
	o.w.Header().Set("Content-Type", "text/event-stream")
	o.w.Header().Set("Cache-Control", "no-cache")
	o.w.Header().Set("X-Accel-Buffering", "no")
	if o.q.responses {
		for _, s := range []string{"created", "in_progress"} {
			o.send("response."+s, obj{"response": o.response("in_progress", nil)})
		}
	} else {
		o.chunk(obj{"role": "assistant", "content": ""}, nil)
	}
}
func (o *output) itemEvent(name string, i int) {
	o.send("response.output_item."+name, obj{"output_index": i, "item": o.items[i]})
}
func (o *output) partEvent(kind string, v obj) {
	i := len(o.items) - 1
	v["output_index"] = i
	v["item_id"] = o.items[i]["id"]
	if o.active == "reasoning" {
		v["summary_index"] = 0
	} else {
		v["content_index"] = 0
	}
	o.send("response."+kind, v)
}
func (o *output) closePart() {
	if o.active == "" {
		return
	}
	i := len(o.items) - 1
	item := o.items[i]
	s := o.part.String()
	if o.active == "reasoning" {
		part := obj{"type": "summary_text", "text": s}
		item["summary"] = []obj{part}
		o.partEvent("reasoning_summary_text.done", obj{"text": s})
		o.partEvent("reasoning_summary_part.done", obj{"part": part})
	} else {
		part := obj{"type": "output_text", "text": s, "annotations": []any{}}
		item["content"] = []obj{part}
		item["status"] = "completed"
		o.partEvent("output_text.done", obj{"text": s})
		o.partEvent("content_part.done", obj{"part": part})
	}
	o.itemEvent("done", i)
	o.active = ""
	o.part.Reset()
}
func (o *output) delta(thought bool, s string) {
	kind := "message"
	field := "content"
	if thought {
		kind = "reasoning"
		field = "reasoning_content"
		o.thought.WriteString(s)
	} else {
		o.text.WriteString(s)
	}
	if !o.q.responses {
		o.chunk(obj{field: s}, nil)
		return
	}
	if o.active != kind {
		o.closePart()
		o.active = kind
		var item, part obj
		var event string
		if thought {
			item = obj{"id": uid("rs_"), "type": kind, "summary": []obj{}}
			part = obj{"type": "summary_text", "text": ""}
			event = "reasoning_summary_part.added"
		} else {
			item = obj{"id": uid("msg_"), "type": kind, "role": "assistant", "status": "in_progress", "content": []obj{}}
			part = obj{"type": "output_text", "text": "", "annotations": []any{}}
			event = "content_part.added"
		}
		o.items = append(o.items, item)
		o.itemEvent("added", len(o.items)-1)
		o.partEvent(event, obj{"part": part})
	}
	o.part.WriteString(s)
	event := "output_text.delta"
	if thought {
		event = "reasoning_summary_text.delta"
	}
	o.partEvent(event, obj{"delta": s})
}
func (o *output) addCall(c *pending) {
	call := historyCall(c.id, c.name, c.args)
	o.calls = append(o.calls, call)
	if !o.q.responses {
		o.chunk(obj{"tool_calls": []obj{{"index": len(o.calls) - 1, "id": c.id, "type": "function", "function": call["function"]}}}, nil)
		return
	}
	o.closePart()
	i := len(o.items)
	item := obj{"id": uid("fc_"), "call_id": c.id, "type": "function_call", "name": c.name, "arguments": "", "status": "in_progress"}
	o.items = append(o.items, item)
	o.itemEvent("added", i)
	o.send("response.function_call_arguments.delta", obj{"item_id": item["id"], "output_index": i, "delta": c.args})
	item["arguments"] = c.args
	o.send("response.function_call_arguments.done", obj{"item_id": item["id"], "output_index": i, "arguments": c.args, "name": c.name})
	item["status"] = "completed"
	o.itemEvent("done", i)
}
func (o *output) assistant() message {
	m := message{Role: "assistant", Calls: o.calls}
	if o.text.Len() > 0 || len(m.Calls) == 0 {
		m.Blocks = []obj{{"type": "text", "text": o.text.String()}}
	}
	return m
}

// Keep the same item boundaries the client receives, including text before
// a function call and interleaved reasoning summaries.
func (o *output) history() []message {
	if !o.q.responses {
		return []message{o.assistant()}
	}
	o.closePart()
	if !o.q.store {
		return nil
	}
	var history []message
	for _, item := range o.items {
		switch item["type"] {
		case "reasoning":
			for _, part := range item["summary"].([]obj) {
				if text := str(part["text"]); text != "" {
					history = append(history, message{Role: "reasoning", Blocks: []obj{{"type": "text", "text": text}}})
				}
			}
		case "message":
			var blocks []obj
			for _, part := range item["content"].([]obj) {
				blocks = append(blocks, obj{"type": "text", "text": part["text"]})
			}
			history = append(history, message{Role: "assistant", Blocks: blocks})
		case "function_call":
			history = append(history, message{Role: "assistant", Calls: []obj{historyCall(str(item["call_id"]), str(item["name"]), str(item["arguments"]))}})
		}
	}
	return history
}

func usage(u obj, responses bool) any {
	if u == nil {
		return nil
	}
	n := func(k string) any {
		if v := u[k]; v != nil {
			return v
		}
		return 0
	}
	input, output := "prompt_tokens", "completion_tokens"
	if responses {
		input = "input_tokens"
		output = "output_tokens"
	}
	return obj{input: n("inputTokens"), output: n("outputTokens"), "total_tokens": n("totalTokens"), input + "_details": obj{"cached_tokens": n("cachedReadTokens")}, output + "_details": obj{"reasoning_tokens": n("thoughtTokens")}}
}
func (o *output) finish(result obj) {
	finish := "stop"
	switch str(result["stopReason"]) {
	case "max_tokens", "max_turn_requests":
		finish = "length"
	case "refusal":
		finish = "content_filter"
	}
	if len(o.calls) > 0 {
		finish = "tool_calls"
	}
	u := usage(asObj(result["usage"]), o.q.responses)
	if o.q.responses {
		o.closePart()
		status := "completed"
		if finish == "length" || finish == "content_filter" {
			status = "incomplete"
		}
		v := o.response(status, u)
		if status == "incomplete" {
			reason := "max_output_tokens"
			if finish == "content_filter" {
				reason = "content_filter"
			}
			v["incomplete_details"] = obj{"reason": reason}
		}
		if o.q.stream {
			o.send("response."+status, obj{"response": v})
		} else {
			o.streamErr = writeJSON(o.w, 200, v)
		}
		return
	}
	if o.q.stream {
		o.chunk(obj{}, finish)
		if o.q.includeUsage && u != nil {
			v := o.chat("chat.completion.chunk", []obj{})
			v["usage"] = u
			o.send("", v)
		}
		o.done()
		return
	}
	m := obj{"role": "assistant", "content": o.text.String()}
	if o.thought.Len() > 0 {
		m["reasoning_content"] = o.thought.String()
	}
	if len(o.calls) > 0 {
		m["tool_calls"] = o.calls
		if o.text.Len() == 0 {
			m["content"] = nil
		}
	}
	v := o.chat("chat.completion", []obj{{"index": 0, "message": m, "finish_reason": finish}})
	if u != nil {
		v["usage"] = u
	}
	o.streamErr = writeJSON(o.w, 200, v)
}
func (o *output) done() {
	if o.streamErr != nil {
		return
	}
	_, o.streamErr = fmt.Fprint(o.w, "data: [DONE]\n\n")
	if o.streamErr == nil {
		o.streamErr = http.NewResponseController(o.w).Flush()
	}
}
func (o *output) failed(err error) {
	if !o.q.stream {
		writeFailure(o.w, err)
		return
	}
	f := publicFailure(err)
	if o.q.responses {
		if o.active != "" {
			item := o.items[len(o.items)-1]
			item["status"] = "incomplete"
			if o.active == "reasoning" {
				item["summary"] = []obj{{"type": "summary_text", "text": o.part.String()}}
			} else {
				item["content"] = []obj{{"type": "output_text", "text": o.part.String(), "annotations": []any{}}}
			}
		}
		v := o.response("failed", nil)
		code := "server_error"
		if f.status == 429 {
			code = "rate_limit_exceeded"
		}
		v["error"] = obj{"code": code, "message": f.msg}
		o.send("response.failed", obj{"response": v, "gateway_error": obj{"status": f.status, "error": errorBody(f)}})
	} else {
		o.send("", obj{"error": errorBody(f)})
		o.done()
	}
}
