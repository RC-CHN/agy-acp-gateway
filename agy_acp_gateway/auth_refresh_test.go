package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The native client's token URL is fixed. Terminate that single origin locally
// in the loopback namespace, using a test CA trusted only by the synthetic CLI.
func offlineOAuth(t *testing.T, dir string, available *atomic.Bool, attempts *atomic.Int32) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"oauth2.googleapis.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "offline-issuer.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	var issued atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "oauth2.googleapis.com:443" {
			http.Error(w, "offline fixture", 403)
			return
		}
		raw, b, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(4 * time.Second))
		fmt.Fprint(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if err = b.Flush(); err != nil {
			return
		}
		conn := tls.Server(raw, cfg)
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		defer req.Body.Close()
		if req.Method != "POST" || req.URL.Path != "/token" {
			return
		}
		// Discard the body; no client credentials are captured.
		_, _ = io.Copy(io.Discard, req.Body)
		status, body := 503, `{"error":"temporarily_unavailable"}`
		if available.Load() {
			n := issued.Add(1)
			token := obj{"token_type": "Bearer", "access_token": fmt.Sprintf("offline-access-%d", n), "expires_in": 3600}
			if n <= 2 {
				token["refresh_token"] = fmt.Sprintf("offline-refresh-%d", n)
			}
			status, body = 200, enc(token)
		}
		attempts.Add(1)
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", status, http.StatusText(status), len(body), body)
	}))
	t.Cleanup(mock.Close)
	return ca, mock.URL
}

func TestRealIdleCredentialRefresh(t *testing.T) {
	bin := os.Getenv("AGY_REAL_BINARY")
	if bin == "" {
		t.Skip("requires offline native CLI")
	}
	if err := offlineNamespace(); err != nil {
		t.Fatal(err)
	}
	cc := &ccpaCapture{}
	up := httptest.NewServer(cc)
	t.Cleanup(up.Close)
	c := authConfig(t, "fail")
	c.cwd = t.TempDir()
	c.authKeepalive = 200 * time.Millisecond
	cmd, err := offlineCommand(bin, up.URL, "", c.credentialHome, c.cwd)
	if err != nil {
		t.Fatal(err)
	}
	var available atomic.Bool
	var attempts atomic.Int32
	ca, proxy := offlineOAuth(t, c.credentialHome, &available, &attempts)
	for i, pair := range cmd.env {
		k, _, _ := strings.Cut(pair, "=")
		if k == "HTTPS_PROXY" {
			cmd.env[i] = k + "=" + proxy
		}
	}
	cmd.env = append(cmd.env, "SSL_CERT_FILE="+ca)
	c.nativeACP = &cmd
	start := func() (*gateway, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		g := newGateway(ctx, c)
		done := make(chan struct{})
		go func() { defer close(done); supervise(ctx, cmd, g) }()
		stop := func() { cancel(); <-done }
		t.Cleanup(stop)
		until(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.worker != nil })
		return g, stop
	}
	g, stop := start()
	headers := http.Header{"Authorization": {"Bearer fixture"}}
	send := func(g *gateway, q obj) obj {
		t.Helper()
		w := invoke(context.Background(), g, "POST", "/v1/responses", enc(q), headers)
		var v obj
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("model request: %d", w.Code)
		}
		return v
	}
	path := g.auth.credentialPath(credentialFile)
	read := func() obj {
		raw, e := os.ReadFile(path)
		var v obj
		if e != nil || json.Unmarshal(raw, &v) != nil {
			return nil
		}
		return v
	}
	const expiredAt = "2000-01-01T00:00:00Z"
	expire := func() {
		t.Helper()
		record := read()
		asObj(record["token"])["expiry"] = expiredAt
		if err := os.WriteFile(path+".fixture", []byte(enc(record)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".fixture", path); err != nil {
			t.Fatal(err)
		}
	}
	// The gateway has never received an HTTP request, including a health check.
	expire()
	until(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return attempts.Load() > 0 && !g.lastAuthCheck.IsZero()
	})
	g.mu.Lock()
	serving := g.worker != nil && g.authError == nil
	g.mu.Unlock()
	if !serving {
		t.Fatal("idle outage interrupted serving")
	}
	if g.usage.total.Load() != 0 {
		t.Fatal("silent check generated a model request")
	}
	if asObj(read()["token"])["refresh_token"] != "offline-test-refresh" {
		t.Fatal("temporary failure discarded saved credentials")
	}
	available.Store(true)
	timedOut := time.Now().Add(18 * time.Second)
	for {
		token := asObj(read()["token"])
		g.mu.Lock()
		checked := !g.lastAuthCheck.IsZero() && g.authCheckError == ""
		g.mu.Unlock()
		if checked && token["access_token"] == "offline-access-1" && token["refresh_token"] == "offline-refresh-1" {
			break
		}
		if time.Now().After(timedOut) {
			t.Fatal("native refresh was not persisted after recovery", attempts.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if g.usage.total.Load() != 0 || g.usage.failed.Load() != 0 {
		t.Fatal("probe counted as a model request")
	}
	cc.mu.Lock()
	turns := len(cc.turns)
	cc.mu.Unlock()
	if turns != 0 {
		t.Fatalf("background probe generated model turns: %d", turns)
	}
	first := send(g, obj{"input": "first client after idle refresh"})
	renewed := attempts.Load()
	expire()
	until(t, func() bool {
		token := asObj(read()["token"])
		return attempts.Load() > renewed && token["access_token"] == "offline-access-2" && token["refresh_token"] == "offline-refresh-2"
	})
	// Some OAuth responses only replace the access token. The last refresh
	// token must remain usable for the next idle period and process restart.
	expire()
	until(t, func() bool {
		token := asObj(read()["token"])
		return token["access_token"] == "offline-access-3" && token["refresh_token"] == "offline-refresh-2"
	})
	send(g, obj{"previous_response_id": first["id"], "input": "continue after another idle refresh"})
	stop()

	renewals := attempts.Load()
	restarted, stopAgain := start()
	send(restarted, obj{"input": "after restart"})
	if attempts.Load() != renewals {
		t.Fatal("restart failed to reuse refreshed credentials")
	}
	stopAgain()
	// No published worker remains. Logout must still invoke native ACP.
	fresh := newGateway(context.Background(), c)
	method := fresh.auth.credentialPath("cache/acp-auth-state.json")
	if err = os.MkdirAll(filepath.Dir(method), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(method, []byte(`{"method":"oauth-personal"}`), 0600); err != nil {
		t.Fatal(err)
	}
	adminCall(t, fresh, "auth/logout", obj{}, 200, "")
	for _, name := range []string{path, method} {
		if _, err = os.Stat(name); !os.IsNotExist(err) {
			t.Fatal("state survived logout", name)
		}
	}
}
