package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

const maxImageBytes = 20 << 20

var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Schema      obj    `json:"inputSchema"`
}
type message struct {
	Role   string
	Blocks []obj
	Calls  []obj
	ID     string
}
type request struct {
	responses, stream, includeUsage, hasTools, store, clientModel bool
	cache                                                         bool
	model, system, effort, previous, key, user                    string
	history                                                       []message
	tools                                                         []tool
}

func content(v any, images *int) ([]obj, error) {
	if s, ok := v.(string); ok {
		return []obj{{"type": "text", "text": s}}, nil
	}
	parts, ok := v.([]any)
	if !ok {
		return nil, invalid("expected string or content array")
	}
	blocks := make([]obj, 0, len(parts))
	for _, raw := range parts {
		p := asObj(raw)
		switch str(p["type"]) {
		case "text", "input_text", "output_text":
			s, ok := p["text"].(string)
			if !ok {
				return nil, invalid("invalid text part")
			}
			blocks = append(blocks, obj{"type": "text", "text": s})
		case "image_url", "input_image":
			u := str(p["image_url"])
			if u == "" {
				u = str(asObj(p["image_url"])["url"])
			}
			h, data, ok := strings.Cut(u, ",")
			mime := strings.TrimSuffix(strings.TrimPrefix(h, "data:"), ";base64")
			if !ok || h != "data:"+mime+";base64" || !(mime == "image/png" || mime == "image/jpeg" || mime == "image/webp" || mime == "image/gif") {
				return nil, invalid("expected a supported base64 image data URL")
			}
			if len(data) > base64.StdEncoding.EncodedLen(maxImageBytes-*images) {
				return nil, invalid("image size limit exceeded")
			}
			b, err := base64.StdEncoding.Strict().DecodeString(data)
			if err != nil || len(b) == 0 || len(b)+*images > maxImageBytes {
				return nil, invalid("invalid or oversized base64 image")
			}
			*images += len(b)
			blocks = append(blocks, obj{"type": "image", "data": data, "mimeType": mime})
		default:
			return nil, invalid("unsupported content part")
		}
	}
	return blocks, nil
}
func texts(blocks []obj) string {
	var a []string
	for _, b := range blocks {
		a = append(a, str(b["text"]))
	}
	return strings.Join(a, "\n")
}
func normalizeCall(id, name string, args any) (obj, error) {
	if id == "" || !validName.MatchString(name) {
		return nil, toolHistory("invalid function call")
	}
	s, ok := args.(string)
	if !ok {
		s = enc(args)
	}
	var v any
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF {
		v = obj{"_raw": s}
	}
	return historyCall(id, name, enc(v)), nil
}
func parseInput(b obj, h http.Header, responses bool) (q request, err error) {
	q.responses = responses
	for _, field := range []string{"model", "instructions", "previous_response_id", "prompt_cache_key", "reasoning_effort"} {
		if v := b[field]; v != nil {
			if _, ok := v.(string); !ok {
				return q, parameter("invalid_parameter", field, field+" must be a string")
			}
		}
	}
	if v := b["stream"]; v != nil {
		if _, ok := v.(bool); !ok {
			return q, parameter("invalid_parameter", "stream", "stream must be a boolean")
		}
	}
	q.stream, _ = b["stream"].(bool)
	q.includeUsage, _ = asObj(b["stream_options"])["include_usage"].(bool)
	q.model = str(b["model"])
	q.clientModel = q.model != ""
	q.effort = str(b["reasoning_effort"])
	q.previous = str(b["previous_response_id"])
	if responses {
		if b["conversation"] != nil {
			return q, parameter("invalid_parameter", "conversation", "conversation lookup is unavailable; supply input history or previous_response_id")
		}
		q.store = true
		if v := b["store"]; v != nil {
			var ok bool
			q.store, ok = v.(bool)
			if !ok {
				return q, parameter("invalid_parameter", "store", "store must be a boolean")
			}
		}
		q.system = str(b["instructions"])
		if raw := b["reasoning"]; raw != nil {
			reasoning := asObj(raw)
			if reasoning == nil {
				return q, parameter("invalid_parameter", "reasoning", "reasoning must be an object")
			}
			if v := reasoning["effort"]; v != nil {
				effort, ok := v.(string)
				if !ok {
					return q, parameter("invalid_parameter", "reasoning.effort", "reasoning.effort must be a string")
				}
				q.effort = effort
			}
		} else {
			q.effort = ""
		}
	}
	if raw, present := b["tools"]; present {
		q.hasTools = true
		tools, ok := raw.([]any)
		if !ok {
			return q, parameter("invalid_parameter", "tools", "tools must be an array")
		}
		seen := map[string]bool{}
		for _, x := range tools {
			t := asObj(x)
			if str(t["type"]) != "function" {
				return q, parameter("invalid_parameter", "tools", "only function tools are supported")
			}
			if !responses {
				t = asObj(t["function"])
			}
			name := str(t["name"])
			if !validName.MatchString(name) || seen[name] {
				return q, parameter("invalid_parameter", "tools", "invalid or duplicate tool name")
			}
			seen[name] = true
			schema := asObj(t["parameters"])
			if schema == nil {
				if t["parameters"] != nil {
					return q, parameter("invalid_parameter", "tools", "invalid tool schema")
				}
				schema = obj{"type": "object", "properties": obj{}}
			}
			q.tools = append(q.tools, tool{name, str(t["description"]), schema})
		}
	}
	slices.SortFunc(q.tools, func(a, b tool) int { return strings.Compare(a.Name, b.Name) })
	raw := b["messages"]
	if responses {
		raw = b["input"]
		if s, ok := raw.(string); ok {
			raw = []any{obj{"role": "user", "content": s}}
		}
	}
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return q, invalid("expected non-empty messages or input")
	}
	imageBytes := 0
	for _, x := range items {
		m := asObj(x)
		role := str(m["role"])
		msg := message{Role: role}
		v := m["content"]
		if responses {
			switch str(m["type"]) {
			case "", "message":
				if m["tool_calls"] != nil || role == "tool" {
					return q, toolHistory("use function_call and function_call_output items in Responses input")
				}
			case "reasoning":
				summary, e := replaySummary(m)
				if e != nil {
					return q, e
				}
				if len(summary) > 0 {
					q.history = append(q.history, message{Role: "reasoning", Blocks: summary})
				}
				continue
			case "item_reference":
				return q, invalid("item_reference is unavailable; supply the complete item")
			case "function_call_output":
				msg.Role = "tool"
				msg.ID = str(m["call_id"])
				v = m["output"]
			case "function_call":
				id, name := str(m["call_id"]), str(m["name"])
				args, ok := m["arguments"].(string)
				if id == "" || !validName.MatchString(name) || !ok {
					return q, toolHistory("function_call requires call_id, name and string arguments")
				}
				c := historyCall(id, name, args)
				msg.Role = "assistant"
				msg.Calls = []obj{c}
			default:
				return q, invalid("unsupported response input item")
			}
		}
		if !responses && msg.Role == "assistant" && m["tool_calls"] != nil {
			for _, c := range arr(m["tool_calls"]) {
				cm := asObj(c)
				f := asObj(cm["function"])
				c, e := normalizeCall(str(cm["id"]), str(f["name"]), f["arguments"])
				if e != nil {
					return q, e
				}
				msg.Calls = append(msg.Calls, c)
			}
		}
		if !responses && msg.Role == "tool" && msg.ID == "" {
			msg.ID = str(m["tool_call_id"])
		}
		if v != nil {
			msg.Blocks, err = content(v, &imageBytes)
			if err != nil {
				return q, err
			}
		} else if len(msg.Calls) == 0 {
			return q, invalid("missing message content")
		}
		switch msg.Role {
		case "system", "developer":
			for _, p := range msg.Blocks {
				if p["type"] != "text" {
					return q, invalid("system instructions require text")
				}
			}
			if q.system != "" {
				q.system += "\n"
			}
			q.system += texts(msg.Blocks)
			continue
		case "tool":
			for _, b := range msg.Blocks {
				if !responses && b["type"] != "text" {
					return q, invalid("tool output must contain text")
				}
			}
			if !responses {
				msg.Blocks = []obj{{"type": "text", "text": texts(msg.Blocks)}}
			}
			if msg.ID == "" {
				return q, toolHistory("missing tool call ID")
			}
		case "user", "assistant":
		default:
			return q, invalid("unsupported message role")
		}
		if len(msg.Calls) > 0 && texts(msg.Blocks) == "" {
			msg.Blocks = nil
		}
		q.history = append(q.history, msg)
	}
	if len(q.history) == 0 {
		return q, invalid("empty conversation")
	}
	if !responses || q.previous == "" {
		if err = validateReplay(q.history); err != nil {
			return q, err
		}
	}
	if !responses {
		q.user = str(b["user"])
	}
	for _, v := range []string{str(b["prompt_cache_key"]), h.Get("X-Session-Id"), h.Get("X-Conversation-Id")} {
		if v != "" {
			q.key = v
			break
		}
	}
	return q, nil
}
func digest(v any) string { b := sha256.Sum256([]byte(enc(v))); return hex.EncodeToString(b[:]) }
func prefix(history, old []message) bool {
	if len(old) > len(history) {
		return false
	}
	for i, m := range old {
		n := history[i]
		if m.Role != n.Role || m.ID != n.ID || !reflect.DeepEqual(m.Blocks, n.Blocks) || len(m.Calls) != len(n.Calls) {
			return false
		}
		for j, c := range m.Calls {
			d := n.Calls[j]
			if enc(c) == enc(d) {
				continue
			}
			a, b := asObj(c["function"]), asObj(d["function"])
			c1, e1 := normalizeCall(str(c["id"]), str(a["name"]), a["arguments"])
			c2, e2 := normalizeCall(str(d["id"]), str(b["name"]), b["arguments"])
			if e1 != nil || e2 != nil || enc(c1) != enc(c2) {
				return false
			}
		}
	}
	return true
}

// Preserve historical roles and call IDs on rebuild. Image bytes stay in image blocks.
func promptBlocks(history []message) []obj {
	var out []obj
	for i, m := range history {
		if m.Role != "user" || i != len(history)-1 {
			meta := obj{"role": m.Role}
			if m.ID != "" {
				meta["tool_call_id"] = m.ID
			}
			if len(m.Calls) > 0 {
				meta["tool_calls"] = m.Calls
			}
			out = append(out, obj{"type": "text", "text": "Conversation history: " + enc(meta)})
		}
		out = append(out, m.Blocks...)
	}
	return out
}
