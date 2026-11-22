package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	dashboard                                   bool
	mode, agent, credentialMode, credentialHome string
	loginCommand                                func() *exec.Cmd
	authKeepalive                               time.Duration
	nativeACP                                   *command
	cwd, bridgeURL, key, cors                   string
	maxSessions, maxTurns                       int
	idle, toolTimeout, turnTimeout, batch       time.Duration
	bodyLimit                                   int64
	models                                      map[string]string
	softBlockResidualTools                      bool
}

func defaults() config {
	return config{authKeepalive: 30 * time.Minute, dashboard: true, credentialMode: "managed", mode: "mock", agent: cleanAgentName, maxSessions: 64, maxTurns: 8, idle: 20 * time.Minute, toolTimeout: 5 * time.Minute, turnTimeout: 15 * time.Minute, batch: 40 * time.Millisecond, bodyLimit: 32 << 20, softBlockResidualTools: true}
}

// Exact unprefixed built-in tool names that agy v1.5.14 still injects even under
// gateway-clean (excludeDefaultComponents: true):
//   - manage_task: always injected by core.addFundamentalComponents.
//   - list_resources, read_resource: injected by tools.GetDiscoveredMcpTools whenever
//     at least one MCP tool is registered. Client tools are exposed under mcp_vbridge_*,
//     so naming these exact unprefixed built-in tools avoids conflating them with any
//     similarly named client tool.
const (
	softBlockNoToolsPrompt   = "The built-in tool with exact name `manage_task` is disabled and unavailable in this environment; never call `manage_task`."
	softBlockWithToolsPrompt = "The built-in tools with exact names `manage_task`, `list_resources`, and `read_resource` are disabled and unavailable in this environment; never call those exact tool names."
)

func effectiveSystemPrompt(clientSystem string, hasTools, softBlock bool) string {
	if !softBlock {
		return clientSystem
	}
	guard := softBlockNoToolsPrompt
	if hasTools {
		guard = softBlockWithToolsPrompt
	}
	if clientSystem == "" {
		return guard
	}
	return clientSystem + "\n\n" + guard
}

type gateway struct {
	auth                   *authManager
	usage                  counters
	wake                   chan struct{}
	paused                 bool
	runCancel              context.CancelFunc
	runDone                chan struct{}
	defaultModel           string
	denied                 atomic.Uint64
	cfg                    config
	ctx                    context.Context
	mu                     sync.Mutex
	worker                 *peer
	authError              *failure
	hadLogin, retryingAuth bool
	lastModelUse           time.Time
	lastAuthCheck          time.Time
	authCheckError         string
	catalog                []string
	sessions               map[string]*session // MCP capability token -> session
	keys, tips, ids        map[string]*session
	started                time.Time
}
type session struct {
	key, token, mcpID, id, tip, model, system, effort string
	tools                                             []tool
	options                                           []any
	peer                                              *peer
	busy                                              bool // owned by one HTTP handler; released when a tool batch is published
	last                                              time.Time
	history                                           []message
	turn                                              *turn
}
type event struct {
	kind, text string
	call       *pending
	result     obj
	err        error
}
type pending struct {
	id, name, args string
	reply          chan []obj
}
type turn struct {
	ctx       context.Context
	cancel    context.CancelFunc
	abort     context.CancelCauseFunc
	events    chan event
	published map[string]*pending
}

func newGateway(ctx context.Context, c config) *gateway {
	g := &gateway{wake: make(chan struct{}, 1), ctx: ctx, cfg: c, sessions: map[string]*session{}, keys: map[string]*session{}, tips: map[string]*session{}, ids: map[string]*session{}, started: time.Now()}
	g.auth = newAuthManager(g)
	return g
}
func (g *gateway) activeLocked() int {
	n := 0
	for _, s := range g.sessions {
		if s.busy || s.turn != nil {
			n++
		}
	}
	return n
}
func (g *gateway) forgetLocked(s *session) {
	delete(g.sessions, s.token)
	if g.keys[s.key] == s {
		delete(g.keys, s.key)
	}
	delete(g.tips, s.tip)
	delete(g.ids, s.id)
}

func (g *gateway) removeLocked(s *session) {
	if g.sessions[s.token] != s {
		return
	}
	g.forgetLocked(s)
	if s.turn != nil {
		s.turn.cancel()
	}
	if s.id != "" {
		id := s.id
		active := s.turn != nil
		go func() {
			if active {
				s.peer.notify("session/cancel", obj{"sessionId": id})
			}
			closeSession(s.peer, id)
		}()
	}
}
func closeSession(p *peer, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = p.call(ctx, "session/close", obj{"sessionId": id})
}
func (g *gateway) reap() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.sessions {
		if !s.busy && s.turn == nil && time.Since(s.last) >= g.cfg.idle {
			g.removeLocked(s)
		}
	}
}
func (g *gateway) incoming(p *peer, f frame) {
	id := str(f.Params["sessionId"])
	g.mu.Lock()
	s := g.ids[id]
	if s != nil && s.peer != p {
		s = nil
	}
	if f.Method == "session/update" && s != nil && s.turn != nil {
		u := asObj(f.Params["update"])
		kind := str(u["sessionUpdate"])
		if kind == "config_option_update" {
			if options := arr(u["configOptions"]); options != nil {
				s.options = options
			}
		}
		if kind == "agent_thought_chunk" || kind == "agent_message_chunk" {
			e := event{kind: kind, text: str(asObj(u["content"])["text"])}
			select {
			case s.turn.events <- e:
			default:
				s.turn.abort(protocolFailure)
			}
		}
	}
	if len(f.ID) == 0 {
		g.mu.Unlock()
		return
	}
	if f.Method != "session/request_permission" {
		g.mu.Unlock()
		go p.reject(f.ID)
		return
	}
	// The ACP worker supplies the resolved tool name. Arguments and display titles are model controlled.
	tc := asObj(f.Params["toolCall"])
	allow := false
	if s != nil && s.turn != nil && str(tc["toolCallId"]) != "" && tc["kind"] == "other" {
		for _, t := range s.tools {
			if str(tc["name"]) == "mcp_vbridge_"+wireName(t.Name) {
				allow = true
				break
			}
		}
	}
	g.mu.Unlock()
	if !allow {
		g.denied.Add(1)
	}
	kind := "reject_once"
	if allow {
		kind = "allow_once"
	}
	out := obj{"outcome": "cancelled"}
	for _, raw := range arr(f.Params["options"]) {
		o := asObj(raw)
		if str(o["kind"]) == kind && str(o["optionId"]) != "" {
			out = obj{"outcome": "selected", "optionId": o["optionId"]}
			break
		}
	}
	go p.reply(f.ID, obj{"outcome": out})
}
func (g *gateway) setup(ctx context.Context, s *session, clientModel bool) error {
	servers := []obj{}
	if len(s.tools) > 0 {
		servers = append(servers, obj{"type": "http", "name": "vbridge", "url": g.cfg.bridgeURL + "/mcp/" + s.token, "headers": []any{}})
	}
	meta := obj{"disableBuiltInTools": true}
	if sys := effectiveSystemPrompt(s.system, len(s.tools) > 0, g.cfg.softBlockResidualTools); sys != "" {
		meta["antigravity"] = obj{"systemPrompt": obj{"append": sys}}
	}
	// A disconnected HTTP client cannot cancel session/new by ID yet. Keep its
	// capacity reserved until ACP returns the ID so the new worker can be closed.
	setupCtx, cancel := context.WithTimeout(g.ctx, min(45*time.Second, g.cfg.turnTimeout))
	defer cancel()
	v, err := s.peer.call(setupCtx, "session/new", obj{"cwd": g.cfg.cwd, "mcpServers": servers, "_meta": meta})
	if err != nil {
		if setupCtx.Err() == context.DeadlineExceeded {
			s.peer.stop()
		}
		return err
	}
	id := str(v["sessionId"])
	if id == "" {
		return protocolFailure
	}
	g.mu.Lock()
	if g.sessions[s.token] != s {
		g.mu.Unlock()
		go closeSession(s.peer, id)
		return workerLost
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		go closeSession(s.peer, id)
		return err
	}
	s.id = id
	g.ids[id] = s
	s.options = arr(v["configOptions"])
	g.mu.Unlock()
	models := asObj(v["models"])
	if s.model != "" && str(models["currentModelId"]) != s.model {
		var available []string
		for _, x := range arr(models["availableModels"]) {
			available = append(available, str(asObj(x)["modelId"]))
		}
		if len(available) > 0 && !slices.Contains(available, s.model) {
			if !clientModel {
				return protocolFailure
			}
			return modelUnavailable
		}
		result, e := s.peer.call(ctx, "session/set_model", obj{"sessionId": id, "modelId": s.model})
		if e != nil {
			return e
		}
		if options := arr(result["configOptions"]); options != nil {
			g.mu.Lock()
			s.options = options
			g.mu.Unlock()
		}
	}
	return nil
}
func optionValue(raw []any, want string) string {
	for _, x := range raw {
		o := asObj(x)
		v := str(o["value"])
		if v != "" && (strings.EqualFold(v, want) || strings.EqualFold(str(o["name"]), want)) {
			return v
		}
		if sub := optionValue(arr(o["options"]), want); sub != "" {
			return sub
		}
	}
	return ""
}
func (g *gateway) setEffort(ctx context.Context, s *session, q request) error {
	effort := q.effort
	if effort == "" || s.effort == effort {
		return nil
	}
	g.mu.Lock()
	options := s.options
	g.mu.Unlock()
	for _, raw := range options {
		o := asObj(raw)
		id := str(o["id"])
		if id != "thought_level" && str(o["category"]) != "thought_level" {
			continue
		}
		val := optionValue(arr(o["options"]), effort)
		if val == "" {
			break
		}
		_, err := s.peer.call(ctx, "session/set_config_option", obj{"sessionId": s.id, "configId": id, "value": val})
		if err == nil {
			s.effort = effort
		}
		return err
	}
	return q.effortError()
}
func toolSuffix(history []message) ([]message, []message) {
	i := len(history)
	for i > 0 && history[i-1].Role == "tool" {
		i--
	}
	return history[:i], history[i:]
}

type lease struct {
	s             *session
	t             *turn
	fresh, resume bool
	input         []message
}

func (g *gateway) acquire(q *request) (lease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var l lease
	if g.worker == nil || g.authError != nil {
		return l, g.unavailableLocked()
	}
	if mapped, ok := g.cfg.models[q.model]; ok {
		q.model = mapped
	}
	if !q.responses && q.key == "" {
		var first message
		for _, m := range q.history {
			if m.Role == "user" {
				first = m
				break
			}
		}
		model := q.model
		if model == "" {
			model = g.defaultModel
		}
		q.key = "chat/" + digest([]any{model, q.system, q.tools, first})
	}
	var parent *session
	s := g.keys[q.key]
	if q.responses {
		if q.previous != "" {
			s = g.tips[q.previous]
			if s == nil {
				return l, previousUnavailable
			}
			if q.model == "" {
				q.model = s.model
			}
			if !q.hasTools {
				q.tools = s.tools
			}
		} else {
			s = nil
			q.key = uid("resp-session-")
		}
	}

	if q.model == "" && len(g.catalog) > 0 {
		q.model = g.defaultModel
		if q.model == "" {
			q.model = g.catalog[0]
		}
	}
	if s != nil && s.busy {
		return l, problem(429, "session_busy", "Wait for the current turn to finish.")
	}
	if q.responses && s != nil {
		// Validate against the committed parent before resuming or rebuilding.
		full := slices.Concat(s.history, q.history)
		if err := validateReplay(full); err != nil {
			return l, err
		}
		if !q.store {
			// A non-stored child has its own ACP turn. The stored tip remains
			// available for a later continuation under the normal TTL/LRU policy.
			parent = s
			q.history = full
			q.key = uid("resp-session-")
			s = nil
		} else {
			q.key = s.key
		}
	}
	before, outputs := toolSuffix(q.history)
	compatible := s != nil && q.model == s.model && q.system == s.system && enc(q.tools) == enc(s.tools)
	chatLiveResume := !q.responses && s != nil && s.turn != nil && len(s.turn.published) > 0 && compatible && len(before) == len(s.history) && prefix(before, s.history)
	if len(outputs) > 0 && (chatLiveResume || (q.responses && s != nil && len(before) == 0)) {
		if s == nil || s.turn == nil || len(s.turn.published) == 0 {
			return l, toolHistory("no pending tool calls")
		}
		if q.responses && q.system == "" {
			q.system = s.system
			compatible = q.model == s.model && enc(q.tools) == enc(s.tools)
		}
		if !compatible || (!q.responses && (len(before) != len(s.history) || !prefix(before, s.history))) || (q.responses && q.effort != "" && q.effort != s.effort) {
			return l, toolHistory("tool results do not match the pending turn")
		}
		seen := map[string]bool{}
		for _, m := range outputs {
			if seen[m.ID] || s.turn.published[m.ID] == nil {
				return l, toolHistory("duplicate or unknown tool call ID")
			}
			seen[m.ID] = true
		}
		if len(seen) != len(s.turn.published) {
			return l, toolHistory("missing tool call results")
		}
		select {
		case <-s.turn.ctx.Done():
			g.removeLocked(s)
			if q.responses {
				return l, previousUnavailable
			}
			// Chat Completions carries the full transcript, so fall through to replay in a fresh session.
			s = nil
		default:
			s.busy = true
			s.history = append(s.history, outputs...)
			for _, m := range outputs {
				s.turn.published[m.ID].reply <- m.Blocks
			}
			s.turn.published = nil
			g.lastModelUse = time.Now()
			return lease{s: s, t: s.turn, resume: true}, nil
		}
	}
	if !q.responses {
		if len(outputs) == 0 && q.history[len(q.history)-1].Role != "user" {
			return l, invalid("last message must be a user message or tool output")
		}
	}
	input := q.history
	if s != nil {
		if q.responses {
			q.history = append(slices.Clone(s.history), q.history...)
		}
		if compatible && s.turn == nil && prefix(q.history, s.history) && len(q.history) > len(s.history) {
			input = q.history[len(s.history):]
			for _, m := range input {
				if m.Role != "user" {
					compatible = false
					break
				}
			}
		} else {
			compatible = false
		}
		if !compatible {
			if s.turn == nil && g.activeLocked() >= g.cfg.maxTurns {
				return l, capacityExceeded
			}
			input = q.history
			g.removeLocked(s)
			s = nil
		}
	}
	if g.activeLocked() >= g.cfg.maxTurns {
		return l, capacityExceeded
	}
	if s == nil {
		if len(g.sessions) >= g.cfg.maxSessions {
			var oldest *session
			for _, x := range g.sessions {
				if x != parent && !x.busy && x.turn == nil && (oldest == nil || x.last.Before(oldest.last)) {
					oldest = x
				}
			}
			if oldest == nil {
				return l, capacityExceeded
			}
			g.removeLocked(oldest)
		}
		s = &session{key: q.key, token: uid(""), mcpID: uid(""), peer: g.worker, model: q.model, system: q.system, tools: q.tools}
		g.sessions[s.token] = s
		g.keys[s.key] = s
		l.fresh = true
	}
	s.busy = true
	s.history = slices.Clone(q.history)
	deadline, stop := context.WithTimeout(g.ctx, g.cfg.turnTimeout)
	ctx, abort := context.WithCancelCause(deadline)
	s.turn = &turn{ctx: ctx, abort: abort, cancel: func() { abort(context.Canceled); stop() }, events: make(chan event, 256)}
	l.s = s
	l.t = s.turn
	l.input = input
	g.lastModelUse = time.Now()
	return l, nil
}
func (g *gateway) dropTurn(s *session, t *turn, reason error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions[s.token] == s && s.turn == t {
		select {
		case <-s.peer.done:
			// The pending prompt RPC will deliver the terminal worker error.
			return
		default:
		}
		t.abort(reason)
		g.removeLocked(s)
	}
}

func (g *gateway) drop(s *session) { g.mu.Lock(); defer g.mu.Unlock(); g.removeLocked(s) }
func (g *gateway) begin(ctx context.Context, l lease, q request) error {
	if l.resume {
		return nil
	}
	if l.fresh {
		if err := g.setup(ctx, l.s, q.clientModel); err != nil {
			return err
		}
	}
	if err := g.setEffort(ctx, l.s, q); err != nil {
		return err
	}
	go func() {
		var blocks []obj
		if l.fresh {
			blocks = replayBlocks(l.input)
		} else {
			blocks = promptBlocks(l.input)
		}
		v, err := l.s.peer.call(l.t.ctx, "session/prompt", obj{"sessionId": l.s.id, "prompt": blocks})
		if err == nil {
			g.usage.record(asObj(v["usage"]))
			switch str(v["stopReason"]) {
			case "cancelled":
				err = turnCancelled
			case "end_turn", "max_tokens", "max_turn_requests", "refusal":
				if l.s.peer.observeStatus {
					err = observedError(v)
				}
			default:
				err = protocolFailure
			}
		}
		select {
		case l.t.events <- event{kind: "end", result: v, err: err}:
		case <-l.t.ctx.Done():
		}
	}()
	return nil
}
func (g *gateway) finish(l lease, q request, id string, output []message, calls []*pending) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := l.s
	if g.sessions[s.token] != s {
		return workerLost
	}
	if err := l.t.ctx.Err(); err != nil {
		return err
	}
	if q.responses && !q.store {
		if len(calls) == 0 {
			l.t.cancel()
			s.turn = nil
		}
		g.removeLocked(s)
		return nil
	}
	s.history = append(s.history, output...)
	if q.responses {
		delete(g.tips, s.tip)
		s.tip = id
		g.tips[id] = s
	}
	if len(calls) == 0 {
		l.t.cancel()
		s.turn = nil
	} else {
		s.turn.published = map[string]*pending{}
		for _, c := range calls {
			s.turn.published[c.id] = c
		}
	}
	s.busy = false
	s.last = time.Now()
	return nil
}
func (g *gateway) modelsList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.modelsLocked()
}
func (g *gateway) modelsLocked() []string {
	a := append([]string{}, g.catalog...)
	for alias, target := range g.cfg.models {
		if slices.Contains(g.catalog, target) && !slices.Contains(a, alias) {
			a = append(a, alias)
		}
	}
	slices.Sort(a)
	return a
}
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}
