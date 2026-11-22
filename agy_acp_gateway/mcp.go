package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Preserve interoperable names. Reserve gw_ for stable aliases of names that
// would be normalized or truncated by the worker after adding its MCP prefix.
func wireName(name string) string {
	if len(name) <= 48 && !strings.HasPrefix(name, "gw_") && name == strings.ToLower(name) && !strings.Contains(name, "-") {
		return name
	}
	return "gw_" + digest(name)[:24]
}

func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	body := http.MaxBytesReader(w, r.Body, limit)
	defer body.Close()
	if r.ContentLength > limit {
		return problem(413, "request_too_large", "Request body is too large.")
	}
	dec := json.NewDecoder(body)
	dec.UseNumber()
	err := dec.Decode(v)
	if err == nil {
		var extra any
		err = dec.Decode(&extra)
		if err == io.EOF {
			return nil
		}
	}
	var large *http.MaxBytesError
	if errors.As(err, &large) {
		return problem(413, "request_too_large", "Request body is too large.")
	}
	return problem(400, "invalid_json", "Invalid JSON request body.")
}
func (g *gateway) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		w.WriteHeader(403)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/mcp/")
	g.mu.Lock()
	s := g.sessions[token]
	g.mu.Unlock()
	if s == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		w.WriteHeader(405)
		return
	}
	if r.Method == http.MethodDelete {
		if r.Header.Get("Mcp-Session-Id") != s.mcpID {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var f frame
	if err := readJSON(w, r, g.cfg.bodyLimit, &f); err != nil {
		w.WriteHeader(statusOf(err))
		return
	}
	reply := func(v any) { writeJSON(w, 200, obj{"jsonrpc": "2.0", "id": f.ID, "result": v}) }
	rpcErr := func(code int, msg string) {
		writeJSON(w, 200, obj{"jsonrpc": "2.0", "id": f.ID, "error": obj{"code": code, "message": msg}})
	}
	if f.Version != "2.0" || f.Method == "" {
		rpcErr(-32600, "Invalid request")
		return
	}
	if f.Method == "server/discover" {
		rpcErr(-32601, "Method not found")
		return
	}
	if f.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", s.mcpID)
		reply(obj{"protocolVersion": "2025-11-25", "capabilities": obj{"tools": obj{}}, "serverInfo": obj{"name": "vbridge", "version": "1.0"}})
		return
	}
	if r.Header.Get("Mcp-Session-Id") != s.mcpID {
		w.WriteHeader(404)
		return
	}
	if len(f.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	switch f.Method {
	case "ping":
		reply(obj{})
	case "tools/list":
		tools := make([]obj, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, obj{"name": wireName(t.Name), "description": t.Description, "inputSchema": t.Schema, "_meta": obj{"antigravity/alwaysLoad": true}})
		}
		reply(obj{"tools": tools})
	case "tools/call":
		name := str(f.Params["name"])
		ok := false
		for _, tool := range s.tools {
			if wireName(tool.Name) == name {
				name = tool.Name
				ok = true
				break
			}
		}
		if !ok {
			rpcErr(-32602, "Unknown tool")
			return
		}
		args := asObj(f.Params["arguments"])
		if args == nil {
			if f.Params["arguments"] != nil {
				rpcErr(-32602, "Invalid arguments")
				return
			}
			args = obj{}
		}
		g.mu.Lock()
		t := s.turn
		g.mu.Unlock()
		if t == nil {
			rpcErr(-32602, "No active prompt")
			return
		}
		c := &pending{id: uid("call_"), name: name, args: enc(args), reply: make(chan []obj, 1)}
		toolResult := func(s string, bad bool) { reply(obj{"content": []obj{{"type": "text", "text": s}}, "isError": bad}) }
		timer := time.NewTimer(g.cfg.toolTimeout)
		defer timer.Stop()
		select {
		case t.events <- event{kind: "tool", call: c}:
		case <-t.ctx.Done():
			toolResult("Turn cancelled", true)
			return
		case <-timer.C:
			g.dropTurn(s, t, toolWaitTimeout)
			toolResult("Timed out publishing tool call", true)
			return
		case <-r.Context().Done():
			g.dropTurn(s, t, turnCancelled)
			return
		}
		select {
		case v := <-c.reply:
			reply(obj{"content": v, "isError": false})
		case <-timer.C:
			toolResult("Tool call timed out waiting for client", true)
			g.dropTurn(s, t, toolWaitTimeout)
		case <-r.Context().Done():
			g.dropTurn(s, t, turnCancelled)
		case <-t.ctx.Done():
			toolResult("Turn cancelled", true)
			g.dropTurn(s, t, turnCancelled)
		}
	default:
		rpcErr(-32601, "Method not found")
	}
}
