package rtcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/relay"
	"github.com/xtaci/smux"
)

// End-to-end tests use official gost, never a second forwarding server here.
func startOfficial(t *testing.T, address string, enabled bool) context.CancelFunc {
	t.Helper()
	binary := os.Getenv("GOST_V3_BINARY")
	if binary == "" {
		t.Skip("set GOST_V3_BINARY to official gost v3.3.0")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "-L", fmt.Sprintf("relay+tls://user:password@%s?bind=%t", address, enabled))
	f, err := os.CreateTemp(t.TempDir(), "official-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			f.Close()
			if t.Failed() {
				b, _ := os.ReadFile(f.Name())
				t.Log(string(b))
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, e := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if e == nil {
			c.Close()
			break
		}
		select {
		case <-done:
			t.Fatal("official gost exited at startup")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal(e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return stop
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

type events struct{ bound, failed, peer chan string }

func (*events) Enabled(context.Context, slog.Level) bool { return true }
func (e *events) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		var q chan string
		switch a.Key {
		case "bind":
			q = e.bound
		case "error":
			q = e.failed
		case "peer":
			q = e.peer
		}
		if q != nil {
			select {
			case q <- fmt.Sprint(a.Value.Any()):
			default:
			}
		}
		return true
	})
	return nil
}
func (e *events) WithAttrs([]slog.Attr) slog.Handler { return e }
func (e *events) WithGroup(string) slog.Handler      { return e }

func startClient(t *testing.T, cfg Config, target string) (*Client, *events, context.CancelFunc) {
	t.Helper()
	e := &events{make(chan string, 32), make(chan string, 32), make(chan string, 64)}
	cfg.Logger = slog.New(e)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	client, err := NewClient(cfg, target)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- client.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Run shutdown: %v", err)
				}
			case <-time.After(4 * time.Second):
				t.Error("Run did not release handlers")
			}
		})
	}
	t.Cleanup(stop)
	return client, e, stop
}

func runClient(t *testing.T, cfg Config, target string) (*events, context.CancelFunc) {
	_, e, stop := startClient(t, cfg, target)
	return e, stop
}

func event(t *testing.T, q <-chan string) string {
	t.Helper()
	select {
	case v := <-q:
		return v
	case <-time.After(8 * time.Second):
		t.Fatal("expected client event was not emitted")
		return ""
	}
}

func config(server string) Config {
	return Config{Server: server, Bind: "127.0.0.1:0", User: url.UserPassword("user", "password")}
}

func startEcho(t *testing.T) net.Listener {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln
}

func checkEcho(address string, payload []byte) error {
	c, e := net.DialTimeout("tcp", address, 2*time.Second)
	if e != nil {
		return e
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Second))
	written := make(chan error, 1)
	go func() { _, e := c.Write(payload); written <- e }()
	b := make([]byte, len(payload))
	_, e = io.ReadFull(c, b)
	if e != nil {
		return e
	}
	if e = <-written; e != nil {
		return e
	}
	if !bytes.Equal(payload, b) {
		return errors.New("payload corrupted or Relay header leaked")
	}
	return nil
}

func TestOfficialConcurrentForwarding(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	target := startEcho(t)
	e, stop := runClient(t, config(proxy), target.Addr().String())
	bound := event(t, e.bound)
	if strings.HasSuffix(bound, ":0") {
		t.Fatal("allocated bind port missing")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := checkEcho(bound, bytes.Repeat([]byte{byte(i), 0, 255, 13, 10}, 65536)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ln, err := net.Listen("tcp", bound)
		if err == nil {
			ln.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("remote port not released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOfficialPeerAndIndependentStreams(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	target := startEcho(t)
	e, stop := runClient(t, config(proxy), target.Addr().String())
	bound := event(t, e.bound)
	c, err := net.Dial("tcp", bound)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if peer := event(t, e.peer); peer != c.LocalAddr().String() {
		t.Fatalf("visitor metadata %s != %s", peer, c.LocalAddr())
	}
	if err := checkEcho(bound, []byte("sibling")); err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("alive"))
	b := make([]byte, 5)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "alive" {
		t.Fatalf("sibling close killed live stream: %q %v", b, err)
	}
	stop()
	if _, err := c.Read(b); err == nil {
		t.Fatal("cancellation left stream open")
	}
}

func TestOfficialFailures(t *testing.T) {
	for _, tc := range []struct {
		name           string
		enabled        bool
		password, bind string
		status         uint8
	}{
		{"auth", true, "bad", "", relay.StatusUnauthorized},
		{"bind_disabled", false, "password", "", relay.StatusForbidden},
		{"port_occupied", true, "password", "occupied", relay.StatusServiceUnavailable},
		{"empty_host_defect", true, "password", ":8080", relay.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := freeAddr(t)
			startOfficial(t, proxy, tc.enabled)
			cfg := config(proxy)
			cfg.User = url.UserPassword("user", tc.password)
			if tc.bind == "occupied" {
				cfg.Bind = startEcho(t).Addr().String()
			} else if tc.bind != "" {
				cfg.Bind = tc.bind
			}
			e, _ := runClient(t, cfg, "127.0.0.1:80")
			if msg := event(t, e.failed); !strings.Contains(msg, fmt.Sprintf("status 0x%02x", tc.status)) {
				t.Fatalf("wrong rejection: %s", msg)
			}
		})
	}
}

func TestOfficialReconnectAndTargetFailure(t *testing.T) {
	proxy := freeAddr(t)
	kill := startOfficial(t, proxy, true)
	target := startEcho(t)
	cfg := config(proxy)
	cfg.Bind = freeAddr(t)
	e, _ := runClient(t, cfg, target.Addr().String())
	bound := event(t, e.bound)
	if err := checkEcho(bound, []byte("before")); err != nil {
		t.Fatal(err)
	}
	kill()
	startOfficial(t, proxy, true)
	event(t, e.bound)
	if err := checkEcho(bound, []byte("after")); err != nil {
		t.Fatal(err)
	}
	target.Close()
	c, err := net.Dial("tcp", bound)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("failed target kept stream open")
	}
}

func TestOfficialTLSVerification(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	cfg := config(proxy)
	cfg.TLS = &tls.Config{}
	e, _ := runClient(t, cfg, "127.0.0.1:80")
	if msg := event(t, e.failed); !strings.Contains(msg, "certificate") {
		t.Fatalf("expected TLS verification error: %s", msg)
	}
}

func TestCancelStalledTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	go func() {
		c, e := ln.Accept()
		if e == nil {
			defer c.Close()
			close(accepted)
			io.Copy(io.Discard, c)
		}
	}()
	_, stop := runClient(t, config(ln.Addr().String()), "127.0.0.1:80")
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("client never dialed")
	}
	stop()
}

func TestOfficialHalfClose(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e == nil {
			defer c.Close()
			io.ReadAll(c)
			c.Write([]byte("late-response"))
		}
	}()
	e, _ := runClient(t, config(proxy), ln.Addr().String())
	bound := event(t, e.bound)
	a, _ := net.ResolveTCPAddr("tcp", bound)
	c, err := net.DialTCP("tcp", nil, a)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("request"))
	c.CloseWrite()
	body, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		t.Fatalf("half-close behavior changed: %q", body)
	}
}

// One scripted TLS response/stream for optional headers or stalls. This has no
// BIND/listener/forwarding implementation and is not a deliverable server.
func scriptedPeer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	cert := httptest.NewTLSServer(nil)
	cfg := cert.TLS.Clone()
	cert.Close()
	ln, e := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		handle(c)
	}()
	return ln.Addr().String()
}

func TestOptionalTargetAndPeerHeader(t *testing.T) {
	wrong := startEcho(t)
	right := startEcho(t)
	payload := []byte("header-must-not-leak")
	done := make(chan error, 1)
	proxy := scriptedPeer(t, func(c net.Conn) {
		var req relay.Request
		if _, e := req.ReadFrom(c); e != nil {
			done <- e
			return
		}
		af := &relay.AddrFeature{}
		af.ParseFrom("127.0.0.1:8080")
		r := relay.Response{Version: relay.Version1, Status: relay.StatusOK, Features: []relay.Feature{af}}
		r.WriteTo(c)
		s, e := smux.Client(c, smux.DefaultConfig())
		if e != nil {
			done <- e
			return
		}
		defer s.Close()
		stream, e := s.OpenStream()
		if e != nil {
			done <- e
			return
		}
		defer stream.Close()
		peer := &relay.AddrFeature{}
		peer.ParseFrom("203.0.113.8:4567")
		target := &relay.AddrFeature{}
		target.ParseFrom(right.Addr().String())
		r.Features = []relay.Feature{peer, target}
		r.WriteTo(stream)
		stream.Write(payload)
		b := make([]byte, len(payload))
		_, e = io.ReadFull(stream, b)
		if e == nil && !bytes.Equal(b, payload) {
			e = errors.New("peer header leaked into data")
		}
		done <- e
	})
	wrong.Close()
	e, _ := runClient(t, config(proxy), wrong.Addr().String())
	event(t, e.bound)
	if peer := event(t, e.peer); peer != "203.0.113.8:4567" {
		t.Fatalf("lost peer: %s", peer)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("stream stalled")
	}
}

func TestCancelStalledBind(t *testing.T) {
	ready := make(chan struct{})
	proxy := scriptedPeer(t, func(c net.Conn) { var req relay.Request; req.ReadFrom(c); close(ready); io.Copy(io.Discard, c) })
	_, stop := runClient(t, config(proxy), "127.0.0.1:80")
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("BIND was not sent")
	}
	stop()
}
