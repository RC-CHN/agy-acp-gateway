package main

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := uid("req_")
	w.Header().Set("X-Request-Id", rid)
	path := r.URL.Path
	isAdmin := strings.HasPrefix(path, "/admin/api/")
	if isAdmin || path == "/" {
		adminHeaders(w)
		if !g.cfg.dashboard {
			writeFailure(w, problem(404, "not_found", "Endpoint not found."))
			return
		}
		if path == "/" {
			if r.Method != "GET" {
				w.Header().Set("Allow", "GET")
				writeFailure(w, problem(405, "method_not_allowed", "Method not allowed."))
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
			_, _ = w.Write(dashboardHTML)
			return
		}
	}
	modelPost := r.Method == "POST" && (path == "/v1/chat/completions" || path == "/v1/responses")
	successful := false
	if modelPost {
		g.usage.total.Add(1)
		defer func() {
			if !successful {
				g.usage.failed.Add(1)
			}
		}()
	}
	if (path == "/health" || path == "/ready") && r.Method == "GET" {
		g.mu.Lock()
		ready := g.worker != nil && g.authError == nil
		active := g.activeLocked()
		pending := 0
		evictable := len(g.sessions) < g.cfg.maxSessions
		for _, s := range g.sessions {
			if !s.busy && s.turn == nil {
				evictable = true
			}
			if s.turn != nil {
				pending += len(s.turn.published)
			}
		}
		code := 200
		if path == "/ready" && (!ready || active >= g.cfg.maxTurns || !evictable) {
			code = 503
		}
		v := obj{"ok": code == 200, "acp_alive": ready, "auth_required": g.needsAuthLocked(), "active_sessions": len(g.sessions), "denied_permissions": g.denied.Load(), "pending_tool_calls": pending, "max_sessions": g.cfg.maxSessions, "uptime_s": int(time.Since(g.started).Seconds())}
		g.mu.Unlock()
		writeJSON(w, code, v)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !isAdmin {
		for _, allowed := range strings.Split(g.cfg.cors, ",") {
			if origin == strings.TrimSpace(allowed) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Session-Id, X-Conversation-Id")
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
				break
			}
		}
		if r.Method == "OPTIONS" && w.Header().Get("Access-Control-Allow-Origin") != "" {
			w.WriteHeader(204)
			return
		}
	}
	httpErr := func(e error) {
		f := publicFailure(e)
		slog.Info("request failed", "request_id", rid, "status", f.status, "code", f.code, "upstream", f.upstream)
		writeFailure(w, f)
	}
	if g.cfg.key != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+g.cfg.key)) != 1 {
		httpErr(problem(401, "invalid_api_key", "Provide a valid gateway API key."))
		return
	}
	if isAdmin {
		g.admin(w, r)
		return
	}
	if path == "/v1/models" {
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			httpErr(problem(405, "method_not_allowed", "Method not allowed."))
			return
		}
		models := g.modelsList()
		if len(models) == 0 {
			g.mu.Lock()
			err := g.unavailableLocked()
			g.mu.Unlock()
			httpErr(err)
			return
		}
		data := []obj{}
		for _, m := range models {
			data = append(data, obj{"id": m, "object": "model", "owned_by": "agy"})
		}
		writeJSON(w, 200, obj{"object": "list", "data": data})
		return
	}
	if path != "/v1/chat/completions" && path != "/v1/responses" {
		httpErr(problem(404, "not_found", "Endpoint not found."))
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		httpErr(problem(405, "method_not_allowed", "Method not allowed."))
		return
	}
	var b obj
	if err := readJSON(w, r, g.cfg.bodyLimit, &b); err != nil {
		httpErr(err)
		return
	}
	q, err := parseInput(b, r.Header, path == "/v1/responses")
	if err != nil {
		httpErr(requestFailure(err, r.Context(), q, lease{}))
		return
	}
	started := time.Now()
	l, err := g.acquire(&q)
	if err != nil {
		httpErr(requestFailure(err, r.Context(), q, l))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.cfg.turnTimeout)
	defer cancel()
	// An HTTP disconnect also interrupts session initialization.
	if err = g.begin(ctx, l, q); err != nil {
		f := requestFailure(err, ctx, q, l)
		g.requireAuth(l.s.peer, f)
		g.drop(l.s)
		l.t.cancel()
		httpErr(f)
		return
	}
	o := newOutput(w, q)
	o.start()
	result, calls, err := collect(ctx, l.t, o, g.cfg.batch)
	if err == nil {
		for _, c := range calls {
			o.addCall(c)
		}
		history := o.history()
		err = o.streamErr
		if err == nil {
			err = g.finish(l, q, o.id, history, calls)
		}
	}
	if err != nil {
		f := requestFailure(err, ctx, q, l)
		g.requireAuth(l.s.peer, f)
		g.drop(l.s)
		l.t.cancel()
		o.failed(f)
		slog.Info("request failed", "request_id", rid, "endpoint", path, "status", f.status, "code", f.code, "upstream", f.upstream, "latency_ms", time.Since(started).Milliseconds())
		return
	}
	o.finish(result)
	successful = o.streamErr == nil && r.Context().Err() == nil
	if !successful {
		slog.Info("request delivery interrupted", "request_id", rid, "endpoint", path)
		return
	}
	slog.Info("request", "request_id", rid, "endpoint", path, "status", 200, "latency_ms", time.Since(started).Milliseconds(), "tool_calls", len(calls))
}
func collect(ctx context.Context, t *turn, o *output, batch time.Duration) (obj, []*pending, error) {
	var calls []*pending
	var timer *time.Timer
	var deadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if o.streamErr != nil {
			return nil, nil, o.streamErr
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-t.ctx.Done():
			return nil, nil, context.Cause(t.ctx)
		case <-deadline:
			return nil, calls, nil
		case e := <-t.events:
			switch e.kind {
			case "agent_thought_chunk", "agent_message_chunk":
				o.delta(e.kind == "agent_thought_chunk", e.text)
			case "tool":
				calls = append(calls, e.call)
				if timer == nil {
					timer = time.NewTimer(batch)
					deadline = timer.C
				}
			case "end":
				return e.result, nil, e.err
			}
		}
	}
}
