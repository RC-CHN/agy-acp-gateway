package main

import (
	_ "embed"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

type counters struct{ total, failed, input, output, cached, reasoning atomic.Uint64 }

func (c *counters) record(u obj) {
	add := func(dst *atomic.Uint64, field string) {
		if n, ok := u[field].(float64); ok && n > 0 && n <= 1<<53 && n == math.Trunc(n) {
			dst.Add(uint64(n))
		}
	}
	add(&c.input, "inputTokens")
	add(&c.output, "outputTokens")
	add(&c.cached, "cachedReadTokens")
	add(&c.reasoning, "thoughtTokens")
}
func (c *counters) snapshot() obj {
	return obj{"requests_errors": c.failed.Load(), "requests_total": c.total.Load(), "input_tokens": c.input.Load(), "output_tokens": c.output.Load(), "cached_tokens": c.cached.Load(), "reasoning_tokens": c.reasoning.Load()}
}

func (g *gateway) adminStatus() obj {
	a := g.auth
	a.mu.Lock()
	pendingLogin := a.current != nil
	a.mu.Unlock()
	account, accountErr := a.account()
	g.mu.Lock()
	defer g.mu.Unlock()
	alive := g.worker != nil && g.authError == nil
	status := "connecting"
	switch {
	case g.cfg.mode != "live" && g.cfg.loginCommand == nil:
		status = "mock"
	case g.needsAuthLocked() && !g.retryingAuth:
		status = "unauthenticated"
	case alive:
		status = "authenticated"
	case g.retryingAuth || g.hadLogin || accountErr == nil:
		status = "recovering"
	}
	if status != "unauthenticated" && a.reportedEmail != "" && sameCredentialFile(a.fileAtLogin, account.file) {
		account.Email = a.reportedEmail
	}
	pending := 0
	for _, s := range g.sessions {
		if s.turn != nil {
			pending += len(s.turn.published)
		}
	}
	var lastCheck *time.Time
	if !g.lastAuthCheck.IsZero() {
		stamp := g.lastAuthCheck.UTC()
		lastCheck = &stamp
	}
	return obj{
		"ok": true, "mode": g.cfg.mode, "agent": g.cfg.agent, "soft_block_residual_tools": g.cfg.softBlockResidualTools,
		"auth": obj{
			"status": status, "auth_required": status == "unauthenticated", "saved_login": g.hadLogin || accountErr == nil,
			"credential_mode": g.cfg.credentialMode, "email": account.Email, "login_in_progress": pendingLogin,
			"keepalive_enabled": g.authKeepaliveEnabled(), "last_silent_check_at": lastCheck, "last_silent_check_error": g.authCheckError,
		},
		"acp":      obj{"alive": alive, "uptime_s": int(time.Since(g.started).Seconds()), "models": g.modelsLocked(), "default_model": g.defaultModel},
		"sessions": obj{"active": len(g.sessions), "max": g.cfg.maxSessions, "pending_tool_calls": pending, "denied_permissions": g.denied.Load()},
		"usage":    g.usage.snapshot(),
	}
}

func adminHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

func (g *gateway) admin(w http.ResponseWriter, r *http.Request) {
	adminHeaders(w)
	if g.cfg.key == "" {
		host := (&url.URL{Host: r.Host}).Hostname()
		if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
			writeFailure(w, problem(403, "forbidden_host", "Access the dashboard through a loopback address."))
			return
		}
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Host, r.Host) || u.Path != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeFailure(w, problem(403, "forbidden_origin", "Use the dashboard on the gateway origin."))
			return
		}
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api/")
	method := "POST"
	switch path {
	case "status":
		method = "GET"
	case "auth/start", "auth/cancel", "auth/complete", "auth/logout":
	default:
		writeFailure(w, problem(404, "not_found", "Endpoint not found."))
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		writeFailure(w, problem(405, "method_not_allowed", "Method not allowed."))
		return
	}
	if path == "status" {
		writeJSON(w, 200, g.adminStatus())
		return
	}
	if err := g.auth.allowed(); err != nil {
		writeFailure(w, err)
		return
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "application/json" {
		writeFailure(w, problem(415, "unsupported_media_type", "Use application/json."))
		return
	}
	var body *struct {
		ID   string `json:"login_id"`
		Code string `json:"code"`
	}
	if err := readJSON(w, r, 64<<10, &body); err != nil {
		writeFailure(w, err)
		return
	}
	if body == nil {
		writeFailure(w, problem(400, "invalid_json", "Expected a JSON object."))
		return
	}
	reply := obj{"ok": true}
	var err error
	switch path {
	case "auth/start":
		reply, err = g.auth.start(r.Context())
	case "auth/complete":
		reply, err = g.auth.complete(body.ID, body.Code)
	case "auth/cancel":
		err = g.auth.cancel(body.ID)
	case "auth/logout":
		err = g.auth.logout()
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, 200, reply)
}
