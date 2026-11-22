package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func run(ctx context.Context) error {
	c := defaults()
	var bind, bin, endpoint, agent, modelMap, observerPython, fridaPython string
	var live bool
	flag.BoolVar(&live, "live", false, "use the explicitly configured agy executable and its native authenticated upstream")
	flag.StringVar(&bind, "listen", "127.0.0.1:8080", "public HTTP address")
	flag.StringVar(&bin, "agy", "", "real ACP executable (default uses bundled offline mock)")
	flag.StringVar(&observerPython, "agy-status-python", "", "optional Python 3.11+ interpreter for read-only pinned agy trajectory errors")
	flag.StringVar(&fridaPython, "agy-frida-python", "", "experimental alternative for loopback mock CCPA using Frida")
	flag.StringVar(&endpoint, "mock-ccpa", "", "loopback mock CCPA URL for the real worker")
	flag.StringVar(&agent, "agent", "", "ACP agent name (default auto-provisions minimal gateway-clean agent without built-in coding tools or default prompt; use \"default\" for agy's built-in agent)")
	flag.StringVar(&modelMap, "model-map", "{}", "JSON object mapping aliases to model IDs")
	c.key = os.Getenv("GATEWAY_API_KEY")
	flag.Func("api-key", "Bearer API key (defaults to GATEWAY_API_KEY)", func(value string) error {
		c.key = value
		return nil
	})
	flag.StringVar(&c.cors, "cors-origins", "", "comma-separated allowed origins")
	flag.IntVar(&c.maxSessions, "max-sessions", c.maxSessions, "maximum sessions")
	flag.IntVar(&c.maxTurns, "max-concurrent-turns", c.maxTurns, "maximum active turns including pending tools")
	flag.DurationVar(&c.idle, "session-idle-ttl", c.idle, "idle session lifetime")
	flag.DurationVar(&c.toolTimeout, "tool-timeout", c.toolTimeout, "client tool result timeout")
	flag.DurationVar(&c.turnTimeout, "turn-timeout", c.turnTimeout, "complete ACP prompt timeout")
	flag.Int64Var(&c.bodyLimit, "body-limit", c.bodyLimit, "maximum HTTP body bytes")
	flag.BoolVar(&c.softBlockResidualTools, "soft-block-residual-tools", c.softBlockResidualTools, "append a minimal system instruction telling the model never to call residual agy built-in tools by exact name (manage_task, and list_resources/read_resource when MCP tools are registered)")
	if value := os.Getenv("GATEWAY_DASHBOARD"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("GATEWAY_DASHBOARD must be a boolean")
		}
		c.dashboard = enabled
	}
	if value := os.Getenv("GATEWAY_CREDENTIAL_MODE"); value != "" {
		c.credentialMode = value
	}
	if value := os.Getenv("GATEWAY_AUTH_KEEPALIVE_INTERVAL"); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil {
			return errors.New("GATEWAY_AUTH_KEEPALIVE_INTERVAL must be a duration")
		}
		c.authKeepalive = d
	}
	flag.DurationVar(&c.authKeepalive, "auth-keepalive-interval", c.authKeepalive, "idle interval for silent managed agy credential checks (0 disables)")
	flag.BoolVar(&c.dashboard, "dashboard", c.dashboard, "serve the embedded administration dashboard")
	flag.StringVar(&c.credentialMode, "credential-mode", c.credentialMode, "credential management: managed or external")
	flag.Parse()
	if c.authKeepalive < 0 {
		return errors.New("auth-keepalive-interval must be nonnegative")
	}
	if c.credentialMode != "managed" && c.credentialMode != "external" {
		return errors.New("credential-mode must be managed or external")
	}
	if agent != "" {
		c.agent = agent
	}
	if c.maxSessions < 1 || c.maxTurns < 1 || c.idle <= 0 || c.turnTimeout <= 0 || c.toolTimeout <= 0 || c.bodyLimit < 1 {
		return errors.New("limits must be positive")
	}
	if err := json.Unmarshal([]byte(modelMap), &c.models); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return err
	}
	if !net.ParseIP(host).IsLoopback() && c.key == "" {
		return errors.New("a public listener requires an API key")
	}
	root, err := os.MkdirTemp("", "agy-gateway-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	c.cwd = filepath.Join(root, "workspace")
	if err = os.Mkdir(c.cwd, 0700); err != nil {
		return err
	}
	var cmd command
	if bin != "" {
		if live && endpoint != "" {
			return errors.New("choose either live ACP or mock-ccpa")
		}
		if live {
			cmd, err = nativeCommand(bin, agent, c.cwd)
			if err == nil {
				c.mode = "live"
				c.credentialHome, err = os.UserHomeDir()
				if err == nil && !filepath.IsAbs(c.credentialHome) {
					return errors.New("live mode requires an absolute HOME")
				}
				native := cmd
				c.nativeACP = &native
				c.loginCommand = native.login
			}
		} else {
			c.mode = "mock-ccpa"
			cmd, err = offlineCommand(bin, endpoint, agent, filepath.Join(root, "home"), c.cwd)
		}
		if err != nil {
			return err
		}
		backend := "store"
		if fridaPython != "" {
			if live || observerPython != "" {
				return errors.New("the experimental Frida observer requires -mock-ccpa and a single observer")
			}
			observerPython, backend = fridaPython, "frida"
		}
		if observerPython != "" {
			cmd, err = withStatusObserver(cmd, observerPython, backend)
			if err != nil {
				return err
			}
		}
	} else {
		if observerPython != "" || fridaPython != "" {
			return errors.New("status observation requires -agy")
		}
		if endpoint != "" || agent != "" || live {
			return errors.New("live, mock-ccpa and agent require -agy")
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd = command{bin: exe, args: []string{"mock-acp"}, env: []string{"HOME=" + root}, dir: c.cwd}
	}
	bridge, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer bridge.Close()
	c.bridgeURL = "http://" + bridge.Addr().String()
	public, err := net.Listen("tcp", bind)
	if err != nil {
		return err
	}
	defer public.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := newGateway(ctx, c)
	internal := &http.Server{Handler: http.HandlerFunc(g.mcp), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	api := &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); supervise(ctx, cmd, g) }()
	errCh := make(chan error, 2)
	go func() { errCh <- internal.Serve(bridge) }()
	go func() { errCh <- api.Serve(public) }()
	slog.Info("listening", "address", public.Addr().String(), "mock_acp", bin == "")
	ticker := time.NewTicker(min(time.Minute, c.idle))
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err = <-errCh:
			break loop
		case <-ticker.C:
			g.reap()
		}
	}
	cancel()
	g.auth.shutdown()
	shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = api.Shutdown(shutdown)
	_ = internal.Shutdown(shutdown)
	_ = api.Close()
	_ = internal.Close()
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "mock-acp" {
		serveMock(os.Stdin, os.Stdout, &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 20 * time.Minute})
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		client := http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
		res, err := client.Get("http://127.0.0.1:8080/health")
		if err != nil || res.StatusCode != 200 {
			os.Exit(1)
		}
		res.Body.Close()
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
