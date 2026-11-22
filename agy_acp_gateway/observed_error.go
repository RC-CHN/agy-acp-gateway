package main

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"runtime"
)

//go:embed observer/bridge.py
var observerBridge string

//go:embed observer/status.js
var observerAgent string

// Explicit flags are strict. An image default can fall back to ordinary ACP
// when the user mounts a newer CLI whose layout has not been verified.
func configureStatusObserver(cmd command, storePython, fridaPython, mode string) (command, error) {
	switch {
	case storePython != "" && fridaPython != "":
		return command{}, errors.New("select one agy status observer")
	case fridaPython != "":
		return withStatusObserver(cmd, fridaPython, "frida")
	case storePython != "":
		return withStatusObserver(cmd, storePython, "store")
	}
	switch mode {
	case "", "off":
		return cmd, nil
	case "frida", "store":
		observed, err := withStatusObserver(cmd, "/usr/bin/python3", mode)
		if err != nil {
			slog.Warn("native status observer unavailable, using ACP", "observer", mode)
			return cmd, nil
		}
		return observed, nil
	default:
		return command{}, errors.New("GATEWAY_STATUS_OBSERVER must be frida, store or off")
	}
}

func withStatusObserver(cmd command, python, backend string) (command, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return command{}, errors.New("the agy status observer requires Linux amd64")
	}
	path, err := exec.LookPath(python)
	if err != nil {
		return command{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return command{}, err
	}
	agent := ""
	if backend == "frida" {
		agent = observerAgent
	}
	args := []string{"-I", "-c", observerBridge, backend, agent, cmd.bin}
	check := exec.Command(path, append(append([]string{}, args...), "--check")...)
	check.Env = cmd.env
	if output, err := check.CombinedOutput(); err != nil {
		return command{}, fmt.Errorf("agy status observer preflight: %s (%w)", output, err)
	}
	cmd.args = append(args, cmd.args...)
	cmd.bin = path
	cmd.observeStatus = true
	cmd.observerMode = backend
	return cmd, nil
}

func observedError(result obj) error {
	if result["gateway_observed_worker_lost"] == true {
		return workerLost
	}
	value, ok := result["gateway_observed_http_status"].(float64)
	if !ok || result["stopReason"] != "end_turn" || value < 400 || value > 599 || math.Trunc(value) != value {
		return nil
	}
	source := int(value)
	var f *failure
	switch source {
	case 401:
		f = problem(503, "upstream_auth_failed", "The model service rejected the gateway credentials.")
	case 403:
		f = problem(403, "upstream_permission_denied", "The gateway account lacks permission for this model.")
	case 429:
		f = problem(429, "upstream_rate_limited", "The model service rate limit was reached.")
	case 503:
		f = problem(503, "upstream_unavailable", "The model service is temporarily unavailable.")
	case 504:
		f = problem(504, "upstream_timeout", "The model service timed out.")
	default:
		f = problem(502, "upstream_error", "The model service could not complete the request.")
	}
	f.upstream = obj{"provider_http_status": source}
	switch provenance := str(result["gateway_observed_source"]); provenance {
	case "frida", "trajectory_store":
		f.upstream["observation_source"] = provenance
	}
	if state := str(result["gateway_observed_provider_status"]); knownProviderStatus(state) {
		f.upstream["provider_status"] = state
	}
	if seconds, ok := result["gateway_observed_retry_after_s"].(float64); ok && (source == 429 || source == 503) && seconds >= 0 && seconds <= 1<<53-1 && math.Trunc(seconds) == seconds {
		n := int64(seconds)
		f.retryAfter = &n
	}
	return f
}

func knownProviderStatus(status string) bool {
	switch status {
	case "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND", "ALREADY_EXISTS", "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE", "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED":
		return true
	}
	return false
}
