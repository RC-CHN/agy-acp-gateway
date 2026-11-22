package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startTestCLI(t *testing.T, bin string, args ...string) (string, *exec.Cmd, *http.Client) {
	t.Helper()
	return startTestCLIEnv(t, bin, nil, args...)
}

func startTestCLIEnv(t *testing.T, bin string, env []string, args ...string) (string, *exec.Cmd, *http.Client) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, bin, append([]string{"-listen", addr}, args...)...)
	cmd.Env = append([]string{"HOME=" + t.TempDir()}, env...)
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer cancel()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	})
	client := &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		r, e := client.Get("http://" + addr + "/ready")
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return addr, cmd, client
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("gateway did not become ready")
	return "", nil, nil
}

// This exercises the installed CLI, its public TCP listener and OS signal handlers.
func TestRealGatewaySignals(t *testing.T) {
	bin := os.Getenv("AGY_GATEWAY_BINARY")
	if bin == "" {
		t.Skip("set AGY_GATEWAY_BINARY to the built gateway for the TCP CLI smoke test")
	}
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			addr, cmd, client := startTestCLI(t, bin)
			res, err := client.Post("http://"+addr+"/v1/responses", "application/json", strings.NewReader(`{"input":"offline-cli"}`))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 || !strings.Contains(string(body), "echo: offline-cli") {
				t.Fatalf("CLI response %s", body)
			}
			// Go may spawn its subprocess on any OS thread.
			children := map[int]bool{}
			files, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", cmd.Process.Pid))
			for _, file := range files {
				b, _ := os.ReadFile(file)
				for _, v := range strings.Fields(string(b)) {
					pid, e := strconv.Atoi(v)
					if e == nil {
						children[pid] = true
					}
				}
			}
			if len(children) == 0 {
				t.Fatal("missing child before shutdown")
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			for pid := range children {
				if syscall.Kill(pid, 0) != syscall.ESRCH {
					t.Fatalf("child %d survived %v", pid, signal)
				}
			}
		})
	}
}
