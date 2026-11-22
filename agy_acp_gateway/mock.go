package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// The offline ACP fixture speaks the same NDJSON and HTTP MCP protocols as the worker.
type mockSession struct {
	url, sid string
	tools    []any
	cancel   context.CancelFunc
}
type mockACP struct {
	mu       sync.Mutex
	sessions map[string]*mockSession
	http     *http.Client
}

func (m *mockACP) mcp(ctx context.Context, s *mockSession, method string, params obj) (obj, error) {
	body := obj{"jsonrpc": "2.0", "method": method, "params": params}
	if !strings.HasPrefix(method, "notifications/") {
		body["id"] = 1
	}
	r, err := http.NewRequestWithContext(ctx, "POST", s.url, strings.NewReader(enc(body)))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if s.sid != "" {
		r.Header.Set("Mcp-Session-Id", s.sid)
	}
	res, err := m.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if method == "initialize" {
		s.sid = res.Header.Get("Mcp-Session-Id")
	}
	if res.StatusCode == 202 {
		return nil, nil
	}
	if res.StatusCode != 200 {
		return nil, errors.New("MCP HTTP error")
	}
	var f frame
	err = json.NewDecoder(res.Body).Decode(&f)
	if err != nil {
		return nil, err
	}
	if f.Error != nil {
		return nil, errors.New("MCP method error")
	}
	var out obj
	err = json.Unmarshal(f.Result, &out)
	return out, err
}
func (m *mockACP) handle(p *peer, f frame) {
	go func() {
		reply := func(v obj) { p.reply(f.ID, v) }
		id := str(f.Params["sessionId"])
		m.mu.Lock()
		s := m.sessions[id]
		m.mu.Unlock()
		switch f.Method {
		case "initialize":
			reply(obj{"protocolVersion": 1, "agentInfo": obj{"name": "offline mock", "version": "1"}})
		case "session/new":
			s = &mockSession{}
			servers := arr(f.Params["mcpServers"])
			if len(servers) > 0 {
				s.url = str(asObj(servers[0])["url"])
				ctx := context.Background()
				_, _ = m.mcp(ctx, s, "server/discover", obj{})
				if _, err := m.mcp(ctx, s, "initialize", obj{}); err != nil {
					p.reject(f.ID)
					return
				}
				_, _ = m.mcp(ctx, s, "notifications/initialized", obj{})
				v, err := m.mcp(ctx, s, "tools/list", obj{})
				if err != nil {
					p.reject(f.ID)
					return
				}
				s.tools = arr(v["tools"])
			}
			id = uid("mock_")
			m.mu.Lock()
			m.sessions[id] = s
			m.mu.Unlock()
			reply(obj{"sessionId": id, "models": obj{"currentModelId": "mock", "availableModels": []obj{{"modelId": "mock", "name": "Mock"}, {"modelId": "mock-alt", "name": "Mock Alternative"}}}, "configOptions": []obj{{"id": "thought_level", "options": []obj{{"value": "low"}, {"value": "high"}}}}})
		case "session/close", "session/cancel":
			m.mu.Lock()
			if s != nil && s.cancel != nil {
				s.cancel()
			}
			if f.Method == "session/close" {
				delete(m.sessions, id)
			}
			m.mu.Unlock()
			if len(f.ID) > 0 {
				reply(obj{})
			}
		case "session/set_model", "session/set_config_option":
			reply(obj{})
		case "session/prompt":
			if s == nil {
				p.reject(f.ID)
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.mu.Lock()
			s.cancel = cancel
			m.mu.Unlock()
			notify := func(kind, value string) {
				p.notify("session/update", obj{"sessionId": id, "update": obj{"sessionUpdate": kind, "content": obj{"type": "text", "text": value}}})
			}
			last := ""
			for _, raw := range arr(f.Params["prompt"]) {
				v := asObj(raw)
				if v["type"] == "text" {
					last = str(v["text"])
				} else if v["type"] == "image" {
					last += " [image]"
				}
			}
			notify("agent_thought_chunk", "mock reasoning")
			notify("future_unknown_update", "ignored")
			if strings.HasPrefix(last, "wait:") {
				<-ctx.Done()
				reply(obj{"stopReason": "cancelled"})
				return
			}
			result := "echo: " + last
			if last == continueReplay {
				if values := mockReplayResults(arr(f.Params["prompt"])); len(values) > 0 {
					result = enc(values)
				}
			}
			if len(s.tools) > 0 && strings.HasPrefix(last, "tools:") {
				type toolReply struct {
					name, value string
					failed      bool
				}
				ch := make(chan toolReply, len(s.tools))
				for _, raw := range s.tools {
					name := str(asObj(raw)["name"])
					go func() {
						v, err := m.mcp(ctx, s, "tools/call", obj{"name": name, "arguments": obj{"city": "Shanghai"}})
						value := "error"
						if err == nil {
							if parts := arr(v["content"]); len(parts) > 0 {
								value = str(asObj(parts[0])["text"])
							}
						}
						ch <- toolReply{name, value, err != nil || v["isError"] == true}
					}()
				}
				results := obj{}
				for range s.tools {
					x := <-ch
					if x.failed {
						reply(obj{"stopReason": "cancelled"})
						return
					}
					results[x.name] = x.value
				}
				result = enc(results)
			}
			notify("agent_message_chunk", result)
			stop := "end_turn"
			if strings.HasPrefix(last, "length:") {
				stop = "max_tokens"
			}
			reply(obj{"stopReason": stop, "usage": obj{"inputTokens": 12, "outputTokens": 5, "cachedReadTokens": 3, "thoughtTokens": 2, "totalTokens": 17}})
		default:
			if len(f.ID) > 0 {
				p.reject(f.ID)
			}
		}
	}()
}
func serveMock(r io.Reader, w io.Writer, client *http.Client) {
	m := &mockACP{sessions: map[string]*mockSession{}, http: client}
	p := newPeer(w, m.handle)
	p.read(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.cancel != nil {
			s.cancel()
		}
	}
}

// The echo model can report tool results supplied in a reconstructed prompt.
func mockReplayResults(blocks []any) obj {
	names := map[string]string{}
	results := obj{}
	for _, raw := range blocks {
		var record obj
		if json.Unmarshal([]byte(str(asObj(raw)["text"])), &record) != nil {
			continue
		}
		for _, rawCall := range arr(record["tool_calls"]) {
			c := asObj(rawCall)
			names[str(c["id"])] = str(asObj(c["function"])["name"])
		}
		if record["type"] == "function_call_output" {
			if parts := arr(record["content"]); len(parts) > 0 {
				results[names[str(record["call_id"])]] = asObj(parts[0])["text"]
			}
		}
	}
	return results
}
