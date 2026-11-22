package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type counters struct{ total, failed, input, output, cached, reasoning atomic.Uint64 }

type usageTotals struct {
	Requests  uint64 `json:"requests_total"`
	Errors    uint64 `json:"requests_errors"`
	Input     uint64 `json:"input_tokens"`
	Output    uint64 `json:"output_tokens"`
	Cached    uint64 `json:"cached_tokens"`
	Reasoning uint64 `json:"reasoning_tokens"`
}

func (a *usageTotals) add(b usageTotals) {
	a.Requests += b.Requests
	a.Errors += b.Errors
	a.Input += b.Input
	a.Output += b.Output
	a.Cached += b.Cached
	a.Reasoning += b.Reasoning
}

func (c *counters) totals() usageTotals {
	return usageTotals{c.total.Load(), c.failed.Load(), c.input.Load(), c.output.Load(), c.cached.Load(), c.reasoning.Load()}
}

func (c *counters) snapshot() obj {
	u := c.totals()
	return obj{"requests_total": u.Requests, "requests_errors": u.Errors, "input_tokens": u.Input,
		"output_tokens": u.Output, "cached_tokens": u.Cached, "reasoning_tokens": u.Reasoning}
}

func (c *counters) store(u usageTotals) {
	c.total.Store(u.Requests)
	c.failed.Store(u.Errors)
	c.input.Store(u.Input)
	c.output.Store(u.Output)
	c.cached.Store(u.Cached)
	c.reasoning.Store(u.Reasoning)
}

type usageHistory struct {
	Version int                    `json:"version"`
	Total   usageTotals            `json:"total"`
	Date    string                 `json:"utc_date"`
	Today   usageTotals            `json:"today"`
	Models  map[string]usageTotals `json:"models"`
}

type failureEvent struct {
	At       time.Time `json:"at"`
	Endpoint string    `json:"endpoint"`
	Model    string    `json:"model,omitempty"`
	Status   int       `json:"status"`
	Code     string    `json:"code"`
	Provider int       `json:"provider_http_status,omitempty"`
	Retry    *int64    `json:"retry_after_s,omitempty"`
	ID       string    `json:"request_id"`
}

type cooldown struct {
	LimitedAt time.Time  `json:"last_429_at"`
	Until     *time.Time `json:"cooldown_until,omitempty"`
}

var latencyBounds = [...]float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type telemetry struct {
	mu                     sync.Mutex
	history                usageHistory
	cooldowns              map[string]cooldown
	events                 [20]failureEvent
	next, count            int
	rebuilt, reused, tools uint64
	cancelled, toolTimeout uint64
	turnTimeout            uint64
	latency                [len(latencyBounds)]uint64
	latencySum             float64
	latencyCount           uint64
}

func (t *telemetry) today(now time.Time) {
	if day := now.UTC().Format(time.DateOnly); day != t.history.Date {
		t.history.Date, t.history.Today = day, usageTotals{}
	}
}

func (g *gateway) recordRequest(at time.Time, path, id, model string, f *failure, result obj, l lease) {
	t := &g.telemetry
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.today(now)
	u := asObj(result["usage"])
	token := func(field string) uint64 {
		if v, ok := u[field].(float64); ok && v > 0 && v <= 1<<53 && math.Trunc(v) == v {
			return uint64(v)
		}
		return 0
	}
	delta := usageTotals{Requests: 1, Input: token("inputTokens"), Output: token("outputTokens"), Cached: token("cachedReadTokens"), Reasoning: token("thoughtTokens")}
	if f != nil {
		delta.Errors = 1
		provider, _ := f.upstream["provider_http_status"].(int)
		t.events[t.next] = failureEvent{now.UTC(), path, model, f.status, f.code, provider, f.retryAfter, id}
		t.next = (t.next + 1) % len(t.events)
		t.count = min(len(t.events), t.count+1)
		if model != "" && provider == http.StatusTooManyRequests {
			c := t.cooldowns[model]
			c.LimitedAt = now.UTC()
			if f.retryAfter != nil {
				until := now.UTC().Add(time.Duration(min(*f.retryAfter, math.MaxInt64/int64(time.Second))) * time.Second)
				c.Until = &until
			}
			t.cooldowns[model] = c
		}
		if f.code == "client_disconnected" {
			t.cancelled++
		}
		if f.code == "upstream_timeout" && provider == 0 && l.t != nil && !l.t.timeoutCounted.Swap(true) {
			t.turnTimeout++
		}
	}
	t.history.Total.add(delta)
	t.history.Today.add(delta)
	g.usage.store(t.history.Total)
	if model != "" {
		m := t.history.Models[model]
		m.add(delta)
		t.history.Models[model] = m
	}
	switch {
	case l.resume:
		t.tools++
	case l.fresh:
		t.rebuilt++
	case l.s != nil:
		t.reused++
	}
	seconds := now.Sub(at).Seconds()
	t.latencySum += seconds
	t.latencyCount++
	for i, bound := range latencyBounds {
		if seconds <= bound {
			t.latency[i]++
		}
	}
}

func (t *telemetry) dashboard() obj {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.today(time.Now())
	recent := make([]failureEvent, 0, t.count)
	for i := 1; i <= t.count; i++ {
		recent = append(recent, t.events[(t.next+len(t.events)-i)%len(t.events)])
	}
	models := make(map[string]usageTotals, len(t.history.Models))
	for id, m := range t.history.Models {
		models[id] = m
	}
	cooldowns := make(map[string]cooldown, len(t.cooldowns))
	for id, c := range t.cooldowns {
		cooldowns[id] = c
	}
	return obj{"today": t.history.Today, "utc_date": t.history.Date, "by_model": models, "cooldowns": cooldowns, "recent_errors": recent,
		"session_rebuilt": t.rebuilt, "session_reused": t.reused, "tool_resumed": t.tools,
		"client_cancelled": t.cancelled, "tool_timeouts": t.toolTimeout, "turn_timeouts": t.turnTimeout}
}

func (g *gateway) loadUsage() error {
	path := g.cfg.usageFile
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Surface an unusable mount at startup rather than silently losing totals.
		return g.saveUsage()
	}
	if err != nil {
		return err
	}
	var history usageHistory
	if len(data) > 2<<20 || json.Unmarshal(data, &history) != nil || history.Version != 1 {
		return errors.New("invalid gateway usage file")
	}
	if history.Models == nil {
		history.Models = map[string]usageTotals{}
	}
	g.telemetry.history = history
	g.usage.store(history.Total)
	return nil
}

func (g *gateway) saveUsage() error {
	t := &g.telemetry
	t.mu.Lock()
	defer t.mu.Unlock()
	return g.saveUsageLocked()
}

func (g *gateway) saveUsageLocked() error {
	path := g.cfg.usageFile
	if path == "" {
		return nil
	}
	t := &g.telemetry
	t.today(time.Now())
	f, err := os.CreateTemp(filepath.Dir(path), ".gateway-usage-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(t.history)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (g *gateway) resetUsage() error {
	t := &g.telemetry
	t.mu.Lock()
	defer t.mu.Unlock()
	previous := t.history
	t.history = usageHistory{Version: 1, Models: map[string]usageTotals{}}
	if err := g.saveUsageLocked(); err != nil {
		t.history = previous
		return err
	}
	g.usage.store(usageTotals{})
	// Session diagnostics and the histogram describe the running process.
	return nil
}

func (g *gateway) serveMetrics(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		return problem(405, "method_not_allowed", "Method not allowed.")
	}
	g.mu.Lock()
	sessions, active, alive := len(g.sessions), g.activeLocked(), 0
	if g.worker != nil && g.authError == nil {
		alive = 1
	}
	g.mu.Unlock()
	t := &g.telemetry
	t.mu.Lock()
	var out strings.Builder
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	u := t.history.Total
	for _, counter := range []struct {
		name  string
		value uint64
	}{
		{"requests_total", u.Requests}, {"request_errors_total", u.Errors},
		{"input_tokens_total", u.Input}, {"output_tokens_total", u.Output},
		{"cached_tokens_total", u.Cached}, {"reasoning_tokens_total", u.Reasoning},
		{"session_reused_total", t.reused}, {"session_rebuilt_total", t.rebuilt}, {"tool_resumed_total", t.tools},
		{"client_cancelled_total", t.cancelled}, {"tool_timeouts_total", t.toolTimeout}, {"turn_timeouts_total", t.turnTimeout},
	} {
		fmt.Fprintf(&out, "# TYPE agy_gateway_%s counter\nagy_gateway_%s %d\n", counter.name, counter.name, counter.value)
	}
	rate := 0.0
	if u.Input != 0 {
		rate = float64(u.Cached) / float64(u.Input)
	}
	fmt.Fprintf(&out, "agy_gateway_cache_hit_ratio %g\nagy_gateway_active_sessions %d\nagy_gateway_active_turns %d\nagy_gateway_acp_alive %d\n", rate, sessions, active, alive)
	fmt.Fprintln(&out, "# TYPE agy_gateway_request_duration_seconds histogram")
	for i, bound := range latencyBounds {
		fmt.Fprintf(&out, "agy_gateway_request_duration_seconds_bucket{le=%q} %d\n", fmt.Sprint(bound), t.latency[i])
	}
	fmt.Fprintf(&out, "agy_gateway_request_duration_seconds_bucket{le=\"+Inf\"} %d\nagy_gateway_request_duration_seconds_sum %g\nagy_gateway_request_duration_seconds_count %d\n", t.latencyCount, t.latencySum, t.latencyCount)
	t.mu.Unlock()
	_, _ = w.Write([]byte(out.String()))
	return nil
}
