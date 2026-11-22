package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestObservedStatusClassification(t *testing.T) {
	for source, want := range map[int]int{400: 502, 401: 503, 402: 502, 403: 403, 404: 502, 429: 429, 500: 502, 502: 502, 503: 503, 504: 504, 599: 502} {
		err := observedError(obj{"stopReason": "end_turn", "gateway_observed_http_status": float64(source)})
		if err == nil || statusOf(err) != want || asObj(errorBody(err)["upstream"])["provider_http_status"] != source {
			t.Fatalf("source %d: %v", source, err)
		}
	}
	for _, v := range []any{nil, "403", 403.5, 399.0, 600.0} {
		if err := observedError(obj{"stopReason": "end_turn", "gateway_observed_http_status": v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := observedError(obj{"stopReason": "cancelled", "gateway_observed_http_status": 429.0}); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedStatusObserverBinary(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires Python")
	}
	f := filepath.Join(t.TempDir(), "unknown-build")
	if err = os.WriteFile(f, []byte("unrecognized binary"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = withStatusObserver(command{bin: f}, python, "store")
	if err == nil || (runtime.GOARCH == "amd64" && !strings.Contains(err.Error(), "unsupported agy binary")) {
		t.Fatalf("unexpected preflight result %v", err)
	}
	for _, mode := range []string{"off", "store", "frida"} {
		c, err := configureStatusObserver(command{bin: f}, "", "", mode)
		if err != nil || c.bin != f || c.observeStatus {
			t.Fatalf("image default %q failed to preserve ordinary ACP", mode)
		}
	}
	if _, err := configureStatusObserver(command{bin: f}, python, python, ""); err == nil {
		t.Fatal("conflicting explicit observers")
	}
}

func TestRealObservedStatus(t *testing.T) {
	testRealStatus(t, "-agy-status-python", os.Getenv("AGY_STATUS_PYTHON"))
}

func TestRealFridaStatus(t *testing.T) {
	testRealStatus(t, "-agy-frida-python", os.Getenv("AGY_FRIDA_PYTHON"))
}

func TestRealImageObserver(t *testing.T) {
	if os.Getenv("AGY_VERIFY_IMAGE_DEFAULT") != "1" {
		t.Skip("runs inside the official amd64 gateway image")
	}
	if os.Getenv("GATEWAY_STATUS_OBSERVER") != "frida" {
		t.Fatal("missing image default")
	}
	testRealStatus(t, "", "")
}

func testRealStatus(t *testing.T, option, python string) {
	t.Helper()
	bin, agy := os.Getenv("AGY_GATEWAY_BINARY"), os.Getenv("AGY_REAL_BINARY")
	if bin == "" || agy == "" || (option != "" && python == "") {
		t.Skip("requires gateway, pinned agy and an interpreter for the selected observer")
	}
	if err := offlineNamespace(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	attempts := map[string]int{}
	counts := map[string]int{}
	cancelStarted := make(chan struct{}, 1)
	cc := &ccpaCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			cc.ServeHTTP(w, r)
			return
		}
		var envelope obj
		if json.NewDecoder(r.Body).Decode(&envelope) != nil {
			http.Error(w, "fixture input", 400)
			return
		}
		req := asObj(envelope["request"])
		input := enc(req["contents"])
		status, bodyCode, message := 200, 0, "offline-private-canary"
		title := envelope["requestType"] == "checkpoint"
		key := str(req["sessionId"])
		if title {
			// Make background failures overlap successful and failed model turns.
			status = 403
		} else {
			mu.Lock()
			attempts[key]++
			n := attempts[key]
			switch {
			case strings.Contains(input, "retry"):
				counts["retry"]++
			case strings.Contains(input, "sustained"):
				counts["sustained"]++
			}
			mu.Unlock()
			switch {
			case strings.Contains(input, "cancel-probe"):
				select {
				case cancelStarted <- struct{}{}:
				default:
				}
				status = 429
			case strings.Contains(input, "sustained"):
				status = 429
			case strings.Contains(input, "teapot"):
				status = 418
			case strings.Contains(input, "mismatch"):
				status, bodyCode = 403, 429
			case strings.Contains(input, "forbidden"):
				status = 403
			case strings.Contains(input, "payment"):
				status = 402
			case strings.Contains(input, "credential"):
				status = 401
			case strings.Contains(input, "quota"):
				status, message = 429, "daily quota: offline-private-canary"
			case strings.Contains(input, "retry") && n == 1:
				status = 429
			}
		}
		if status != 200 {
			if bodyCode == 0 {
				bodyCode = status
			}
			e := obj{"code": bodyCode, "status": map[int]string{401: "UNAUTHENTICATED", 402: "RESOURCE_EXHAUSTED", 403: "PERMISSION_DENIED", 429: "RESOURCE_EXHAUSTED"}[bodyCode], "message": message}
			if !title && strings.Contains(input, "quota-invalid-hint") {
				w.Header().Set("Retry-After", "not a date")
				e["status"] = "UNVERIFIED_ENUM"
				e["details"] = []obj{{"@type": "plain_text", "retryDelay": "4321s"}}
			}
			if !title && strings.Contains(input, "quota-hint") {
				e["details"] = []obj{{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "2.25s"}}
				if strings.Contains(input, "header") {
					w.Header().Set("Retry-After", "7")
				}
				w.Header().Set("X-Private", "offline-private-canary")
			}
			writeJSON(w, status, obj{"error": e})
			return
		}
		text := "ordinary assistant text: HTTP 403 quota"
		if strings.Contains(input, "imitate-warning") {
			text = "\n⚠ " + text + "\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\r\n\r\n", enc(obj{"response": obj{"candidates": []obj{{"content": obj{"role": "model", "parts": []obj{{"text": text}}}, "finishReason": "STOP"}}}, "traceId": "offline"}))
	}))
	defer up.Close()
	args := []string{"-agy", agy, "-mock-ccpa", up.URL, "-turn-timeout", "12s"}
	var env []string
	if option != "" {
		args = append(args, option, python)
	} else {
		env = []string{"GATEWAY_STATUS_OBSERVER=" + os.Getenv("GATEWAY_STATUS_OBSERVER")}
	}
	addr, _, client := startTestCLIEnv(t, bin, env, args...)
	run := func(t *testing.T, name string, source, want int, stream, chat bool) {
		t.Helper()
		path := "/v1/responses"
		b := obj{"input": name, "stream": stream, "store": false}
		if chat {
			path = "/v1/chat/completions"
			b = obj{"messages": []any{user(name)}, "stream": stream}
		}
		res, err := client.Post("http://"+addr+path, "application/json", strings.NewReader(enc(b)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		httpWant := want
		if stream {
			httpWant = 200
		}
		if res.StatusCode != httpWant || strings.Contains(body, "offline-private-canary") {
			t.Fatalf("%s HTTP %d, body %.900s", name, res.StatusCode, body)
		}
		if want == 504 && source == 0 {
			if strings.Contains(body, "provider_http_status") {
				t.Fatal(body)
			}
			return
		}
		if strings.Contains(name, "quota-hint") {
			want := 3
			if strings.Contains(name, "header") {
				want = 7
			}
			if !strings.Contains(body, fmt.Sprintf(`"retry_after_s":%d`, want)) || !strings.Contains(body, `"provider_status":"RESOURCE_EXHAUSTED"`) {
				t.Fatalf("missing verified hints: %s", body)
			}
			header := res.Header.Get("Retry-After")
			if (!stream && header != fmt.Sprint(want)) || (stream && header != "") {
				t.Fatal("unexpected Retry-After", header)
			}
		}
		if name == "quota-invalid-hint" && (res.Header.Get("Retry-After") != "" || strings.Contains(body, "retry_after_s") || strings.Contains(body, "provider_status")) {
			t.Fatal(body)
		}
		if name == "mismatch" && strings.Contains(body, "provider_status") {
			t.Fatal("conflicting body status escaped", body)
		}
		if source == 0 {
			if !strings.Contains(body, "ordinary assistant text") || strings.Contains(body, "provider_http_status") {
				t.Fatalf("%s %.900s", name, body)
			}
			return
		}
		if !strings.Contains(body, fmt.Sprintf(`"provider_http_status":%d`, source)) {
			t.Fatalf("%s missing original HTTP %d, body %.1200s", name, source, body)
		}
		if name == "forbidden" && (option == "" || option == "-agy-frida-python") && !strings.Contains(body, `"observation_source":"frida"`) {
			t.Fatal("the Frida observer did not supply the status on the native 403")
		}
		if stream && chat && (strings.Count(body, "[DONE]") != 1 || strings.Contains(body, `"finish_reason":"stop"`)) {
			t.Fatal(body)
		}
		if stream && !chat && (strings.Count(body, "event: response.failed\n") != 1 || strings.Contains(body, "response.completed") || !strings.Contains(body, `"gateway_error"`)) {
			t.Fatal(body)
		}
	}
	t.Run("forbidden", func(t *testing.T) { run(t, "forbidden", 403, 403, false, false) })
	t.Run("credential", func(t *testing.T) { run(t, "credential", 401, 503, false, false) })
	t.Run("mismatched body", func(t *testing.T) { run(t, "mismatch", 403, 403, false, true) })
	t.Run("ordinary text with diagnostic formatting", func(t *testing.T) { run(t, "imitate-warning", 0, 200, true, false) })
	t.Run("same session next prompt", func(t *testing.T) {
		post := func(body obj, want int) obj {
			t.Helper()
			res, err := client.Post("http://"+addr+"/v1/responses", "application/json", strings.NewReader(enc(body)))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var v obj
			if json.NewDecoder(res.Body).Decode(&v) != nil || res.StatusCode != want {
				t.Fatalf("HTTP %d, expected %d", res.StatusCode, want)
			}
			return v
		}
		v := post(obj{"input": "first stored turn", "store": true}, 200)
		if str(v["id"]) == "" {
			t.Fatal("missing response ID")
		}
		v = post(obj{"input": "forbidden follow-up", "previous_response_id": v["id"]}, 403)
		if asObj(asObj(v["error"])["upstream"])["provider_http_status"] != 403.0 {
			t.Fatal("missing original status on follow-up")
		}
	})
	t.Run("responses SSE", func(t *testing.T) { run(t, "forbidden responses stream", 403, 403, true, false) })
	t.Run("chat SSE", func(t *testing.T) { run(t, "forbidden chat stream", 403, 403, true, true) })
	t.Run("402 distinct from 429", func(t *testing.T) { run(t, "payment", 402, 502, false, true) })
	t.Run("other provider error", func(t *testing.T) { run(t, "teapot", 418, 502, false, false) })
	t.Run("unverified retry metadata", func(t *testing.T) { run(t, "quota-invalid-hint", 429, 429, false, false) })
	t.Run("hard quota", func(t *testing.T) { run(t, "quota", 429, 429, false, false) })
	for _, stream := range []bool{false, true} {
		for _, chat := range []bool{false, true} {
			t.Run(fmt.Sprintf("trusted retry/stream=%t/chat=%t", stream, chat), func(t *testing.T) {
				name := "quota-hint"
				if chat {
					name += " header"
				}
				run(t, name, 429, 429, stream, chat)
			})
		}
	}
	t.Run("concurrent sessions", func(t *testing.T) {
		t.Run("retry succeeds", func(t *testing.T) {
			t.Parallel()
			run(t, "retry", 0, 200, false, false)
			mu.Lock()
			defer mu.Unlock()
			if counts["retry"] < 2 {
				t.Fatal("missing model retry")
			}
		})
		t.Run("success despite title", func(t *testing.T) { t.Parallel(); run(t, "success", 0, 200, false, false) })
		t.Run("forbidden", func(t *testing.T) { t.Parallel(); run(t, "concurrent forbidden", 403, 403, false, false) })
	})
	t.Run("timeout while provider retries", func(t *testing.T) {
		run(t, "sustained", 0, 504, false, false)
		mu.Lock()
		defer mu.Unlock()
		if counts["sustained"] < 2 {
			t.Fatal("missing model retry")
		}
	})
	t.Run("cancel during retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/v1/responses", strings.NewReader(enc(obj{"input": "cancel-probe", "store": false})))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		done := make(chan error, 1)
		go func() {
			res, err := client.Do(r)
			if res != nil {
				res.Body.Close()
			}
			done <- err
		}()
		select {
		case <-cancelStarted:
		case <-time.After(6 * time.Second):
			t.Fatal("model attempt did not start")
		}
		cancel()
		if err = <-done; err == nil {
			t.Fatal("cancelled request returned success")
		}
		run(t, "after cancellation", 0, 200, false, false)
	})

}
