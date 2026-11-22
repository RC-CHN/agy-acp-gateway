package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

type failure struct {
	status           int
	code, msg, param string
	upstream         obj
	retryAfter       *int64
}

func (e *failure) Error() string { return e.msg }
func problem(status int, code, msg string) *failure {
	return &failure{status: status, code: code, msg: msg}
}
func parameter(code, field, msg string) *failure {
	return &failure{status: 400, code: code, msg: msg, param: field}
}
func invalid(msg string) error     { return parameter("invalid_parameter", "", msg) }
func toolHistory(msg string) error { return parameter("invalid_tool_history", "", msg) }

var (
	workerLost          = problem(502, "worker_disconnected", "The ACP worker disconnected.")
	workerUnavailable   = problem(503, "worker_unavailable", "The ACP worker is not ready.")
	protocolFailure     = problem(502, "acp_protocol_error", "The ACP worker returned an invalid response.")
	turnCancelled       = problem(502, "acp_request_cancelled", "The ACP worker cancelled the request.")
	turnTimeout         = problem(504, "upstream_timeout", "The ACP turn timed out.")
	internalFailure     = problem(500, "internal_error", "The gateway could not complete the request.")
	previousUnavailable = parameter("previous_response_unavailable", "previous_response_id", "This response can no longer be continued by ID. Supply the full input history.")
	modelUnavailable    = parameter("invalid_model", "model", "The requested model is unavailable.")
	capacityExceeded    = problem(429, "gateway_capacity_exceeded", "Gateway capacity is currently exhausted.")
)

type wireError struct {
	Code    *int            `json:"code"`
	Message *string         `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Retain the wire error and only the request coordinates needed for classification.
// Error() is safe to log even when data or the original message contains credentials.
type rpcFailure struct {
	wire                                      *wireError
	method, sessionID, model, configID, value string
}

func (e *rpcFailure) Error() string {
	return fmt.Sprintf("ACP %s failed (code %d)", e.method, *e.wire.Code)
}

func (e *rpcFailure) classify(q request, continuedSession, selectedModel string) *failure {
	n, message := *e.wire.Code, *e.wire.Message
	f := problem(502, "acp_request_failed", "The ACP worker could not complete the request.")
	sessionCall := e.method == "session/prompt" || e.method == "session/set_model" || e.method == "session/set_config_option"
	switch n {
	case -32000:
		f = problem(503, "acp_auth_required", "ACP authentication required. Sign in to the gateway account.")
	case -32700, -32600, -32601:
		f = problem(502, "acp_protocol_error", "The ACP worker rejected a gateway protocol request.")
	case -32800:
		f = turnCancelled
	case -32602, -32002:
		switch {
		case sessionCall && e.sessionID != "" && e.sessionID == continuedSession && message == "Session not found: "+e.sessionID:
			f = previousUnavailable
		case n == -32002 && sessionCall && e.sessionID != "" && e.sessionID == continuedSession && message == "Session not found":
			f = previousUnavailable
		case n == -32602 && q.clientModel && e.method == "session/set_model" && e.model != "" && message == "Unknown model ID: "+e.model:
			f = modelUnavailable
		case n == -32602 && e.method == "session/set_config_option" && e.value != "" && selectedModel != "" && message == fmt.Sprintf("Thinking level %q is not offered for model %q", e.value, selectedModel):
			f = q.effortError()
		case n == -32602 && ((e.method == "session/set_config_option" && e.configID != "" && message == "Unknown config option: "+e.configID) ||
			(e.method == "session/prompt" && (message == "Empty prompt" || (e.sessionID == "" && message == "Session not found: ")))):
			f = problem(502, "acp_protocol_error", "The ACP worker rejected a gateway protocol request.")
		}
	}
	result := *f
	result.upstream = obj{"acp_code": n}
	switch message {
	case "Authentication required", "Invalid params", "Method not found", "Internal error", "Parse error", "Invalid request", "Empty prompt":
		result.upstream["acp_message"] = message
	}
	return &result
}

func (q request) historyField() string {
	if q.responses {
		return "input"
	}
	return "messages"
}
func (q request) effortError() *failure {
	field := "reasoning_effort"
	if q.responses {
		field = "reasoning.effort"
	}
	return parameter("invalid_reasoning_effort", field, "The requested reasoning effort is unsupported for this model.")
}

func publicFailure(err error) *failure {
	var f *failure
	if errors.As(err, &f) {
		return f
	}
	var rpc *rpcFailure
	if errors.As(err, &rpc) {
		return rpc.classify(request{}, "", "")
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return turnTimeout
	case errors.Is(err, context.Canceled):
		return problem(499, "client_disconnected", "The client disconnected.")
	default:
		return internalFailure
	}
}
func statusOf(err error) int { return publicFailure(err).status }

func errorBody(err error) obj {
	f := publicFailure(err)
	kind := "server_error"
	switch f.status {
	case 400, 404, 405, 409, 413, 415:
		kind = "invalid_request_error"
	case 401:
		kind = "authentication_error"
	case 403:
		kind = "permission_error"
	case 429:
		kind = "rate_limit_error"
	}
	var param any
	if f.param != "" {
		param = f.param
	}
	body := obj{"code": f.code, "message": f.msg, "type": kind, "param": param}
	if f.upstream != nil {
		body["upstream"] = f.upstream
	}
	if f.retryAfter != nil {
		body["retry_after_s"] = *f.retryAfter
	}
	return body
}
func writeFailure(w http.ResponseWriter, err error) {
	f := publicFailure(err)
	if f.retryAfter != nil {
		w.Header().Set("Retry-After", strconv.FormatInt(*f.retryAfter, 10))
	}
	writeJSON(w, f.status, obj{"error": errorBody(f)})
}

// The request's context distinguishes a disconnected caller from a worker-side cancellation.
func requestFailure(err error, ctx context.Context, q request, l lease) *failure {
	if l.t != nil && context.Cause(l.t.ctx) == workerReconnecting {
		return workerReconnecting
	}
	var rpc *rpcFailure
	var f *failure
	if errors.As(err, &rpc) {
		continued := ""
		if q.previous != "" && !l.fresh && l.s != nil {
			continued = l.s.id
		}
		f = rpc.classify(q, continued, q.model)
	} else {
		f = publicFailure(err)
	}
	if f.code == "acp_request_cancelled" || errors.Is(err, context.Canceled) {
		cause := ctx.Err()
		if cause == nil && l.t != nil {
			cause = context.Cause(l.t.ctx)
		}
		switch {
		case errors.Is(cause, context.DeadlineExceeded):
			mapped := *turnTimeout
			mapped.upstream = f.upstream
			return &mapped
		case cause == protocolFailure:
			return protocolFailure
		case ctx.Err() == context.Canceled:
			return publicFailure(context.Canceled)
		case errors.Is(err, context.Canceled):
			return turnCancelled
		}
	}
	if f.param == "" && (f.code == "invalid_tool_history" || f.code == "invalid_parameter") {
		copy := *f
		copy.param = q.historyField()
		return &copy
	}
	return f
}
