package rtcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gost/relay"
	"github.com/xtaci/smux"
)

func listenForTest(t *testing.T, cfg Config) (net.Listener, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ln, err := Listen(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, cancel
}

func acceptVisitor(t *testing.T, ln net.Listener) (net.Conn, net.Conn) {
	t.Helper()
	visitor, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { visitor.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			t.Error(err)
		}
		accepted <- conn
	}()
	select {
	case conn := <-accepted:
		if conn == nil {
			t.Fatal("Accept returned no connection")
		}
		t.Cleanup(func() { conn.Close() })
		visitor.SetDeadline(time.Now().Add(4 * time.Second))
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		return visitor, conn
	case <-time.After(4 * time.Second):
		t.Fatal("Accept blocked")
		return nil, nil
	}
}

func serveEcho(ln net.Listener) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	return done
}

func TestOfficialListenerConcurrentBusiness(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	ln, _ := listenForTest(t, config(proxy))
	if ln.Addr().(*net.TCPAddr).Port == 0 {
		t.Fatal("Listen returned before port allocation")
	}
	done := serveEcho(ln)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := checkEcho(ln.Addr().String(), bytes.Repeat([]byte{byte(i), 0, 255}, 65536)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	ln.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Accept")
	}
}

func TestOfficialListenerConnectionContract(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	ln, cancel := listenForTest(t, config(proxy))
	v1, c1 := acceptVisitor(t, ln)
	v2, c2 := acceptVisitor(t, ln)
	if c1.LocalAddr().String() != ln.Addr().String() || c1.RemoteAddr().String() != v1.LocalAddr().String() {
		t.Fatalf("wrong addresses: %s -> %s", c1.RemoteAddr(), c1.LocalAddr())
	}
	bound := ln.Addr().String()
	ln.Addr().(*net.TCPAddr).IP[0] = 200
	c1.LocalAddr().(*net.TCPAddr).Port = 1
	c1.RemoteAddr().(*net.TCPAddr).Port = 1
	if ln.Addr().String() != bound || c1.LocalAddr().String() != bound || c1.RemoteAddr().String() != v1.LocalAddr().String() {
		t.Fatal("caller mutated shared address state")
	}
	c1.SetReadDeadline(time.Now().Add(-time.Second))
	_, err := c1.Read(make([]byte, 1))
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("read deadline: %v", err)
	}
	c1.SetReadDeadline(time.Time{})
	if _, err := v1.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(c1, b); err != nil || string(b) != "a" {
		t.Fatalf("clear deadline: %q %v", b, err)
	}
	c1.Close()
	c1.Close()
	// Closing/deadlining a sibling must not change this connection.
	ln.Close()
	ln.Close()
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed Accept: %v", err)
	}
	if _, err := c2.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(v2, b); err != nil || string(b) != "b" {
		t.Fatalf("Close interrupted accepted connection: %q %v", b, err)
	}
	cancel()
	if _, err := c2.Read(b); err == nil {
		t.Fatal("context cancellation left accepted connection open")
	}
	if _, err := v2.Read(b); err == nil {
		t.Fatal("context cancellation left visitor open")
	}
}

func TestOfficialListenerCloseReleasesPort(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			proxy := freeAddr(t)
			startOfficial(t, proxy, true)
			ln, _ := listenForTest(t, config(proxy))
			if accepted {
				_, conn := acceptVisitor(t, ln)
				ln.Close()
				conn.Close()
			} else {
				ln.Close()
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				probe, err := net.Listen("tcp", ln.Addr().String())
				if err == nil {
					probe.Close()
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("remote port not released after draining")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestOfficialListenerHTTPShutdown(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	ln, _ := listenForTest(t, config(proxy))
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRequest)
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(started)
				<-release
			}
			fmt.Fprint(w, r.RemoteAddr)
		}),
	}
	t.Cleanup(func() { server.Close() })
	served := make(chan error, 1)
	go func() { served <- server.Serve(ln) }()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var local string
	for i := 0; i < 2; i++ {
		reused := false
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String(), nil)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { local = info.Conn.LocalAddr().String(); reused = info.Reused },
		}))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != local || (i == 1 && !reused) {
			t.Fatalf("HTTP peer/keepalive: %q reused=%t error=%v", body, reused, err)
		}
	}
	response := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + ln.Addr().String() + "/slow")
		if err == nil {
			var body []byte
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != local {
				err = fmt.Errorf("drained response: %q", body)
			}
		}
		response <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not arrive")
	}
	shutdown := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdown <- server.Shutdown(ctx)
	}()
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not stop Serve")
	}
	releaseRequest()
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
}

func TestOfficialListenerReconnect(t *testing.T) {
	proxy := freeAddr(t)
	kill := startOfficial(t, proxy, true)
	cfg := config(proxy)
	e := &events{bound: make(chan string, 32)}
	cfg.Logger = slog.New(e)
	ln, _ := listenForTest(t, cfg)
	event(t, e.bound)
	_, old := acceptVisitor(t, ln)
	oldBound := old.LocalAddr().String()
	kill()
	startOfficial(t, proxy, true)
	bound := event(t, e.bound)
	if bound != ln.Addr().String() || strings.HasSuffix(bound, ":0") {
		t.Fatalf("rebind address not published: %s", bound)
	}
	visitor, conn := acceptVisitor(t, ln)
	old.Close() // Must not decrement the new session's connection count.
	if old.LocalAddr().String() != oldBound {
		t.Fatal("reconnect changed old connection metadata")
	}
	ln.Close()
	if _, err := conn.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 3)
	if _, err := io.ReadFull(visitor, b); err != nil || string(b) != "new" {
		t.Fatalf("reconnected business connection: %q %v", b, err)
	}
}

func TestOfficialListenerStartupErrors(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	cfg := config(proxy)
	cfg.User = url.UserPassword("user", "wrong")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Listen(ctx, cfg); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("status 0x%02x", relay.StatusUnauthorized)) {
		t.Fatalf("authentication failure not returned: %v", err)
	}
	cancel()
	if _, err := Listen(ctx, config(proxy)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Listen: %v", err)
	}
	if _, err := Listen(context.Background(), Config{Server: "invalid"}); err == nil {
		t.Fatal("invalid server accepted")
	}
}

func TestListenerConsumesHeadersAndIgnoresTarget(t *testing.T) {
	done := make(chan error, 1)
	proxy := scriptedPeer(t, func(c net.Conn) {
		var req relay.Request
		if _, err := req.ReadFrom(c); err != nil {
			done <- err
			return
		}
		bound := &relay.AddrFeature{}
		bound.ParseFrom("127.0.0.1:8080")
		resp := relay.Response{Version: relay.Version1, Status: relay.StatusOK, Features: []relay.Feature{bound}}
		resp.WriteTo(c)
		session, err := smux.Client(c, smux.DefaultConfig())
		if err != nil {
			done <- err
			return
		}
		defer session.Close()
		stream, err := session.OpenStream()
		if err != nil {
			done <- err
			return
		}
		defer stream.Close()
		peer, override := &relay.AddrFeature{}, &relay.AddrFeature{}
		peer.ParseFrom("203.0.113.8:4567")
		override.ParseFrom("unreachable.invalid:1234")
		resp.Features = []relay.Feature{peer, override}
		resp.WriteTo(stream)
		stream.Write([]byte("business"))
		b := make([]byte, 8)
		_, err = io.ReadFull(stream, b)
		if err == nil && string(b) != "business" {
			err = fmt.Errorf("header leaked: %q", b)
		}
		done <- err
	})
	ln, _ := listenForTest(t, config(proxy))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.RemoteAddr().String() != "203.0.113.8:4567" {
		t.Fatal(conn.RemoteAddr())
	}
	b := make([]byte, 8)
	if _, err := io.ReadFull(conn, b); err != nil || string(b) != "business" {
		t.Fatalf("business payload: %q %v", b, err)
	}
	conn.Write(b)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestListenCancelStalledBind(t *testing.T) {
	ready := make(chan struct{})
	proxy := scriptedPeer(t, func(c net.Conn) {
		var req relay.Request
		req.ReadFrom(c)
		close(ready)
		io.Copy(io.Discard, c)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		ln, err := Listen(ctx, config(proxy))
		if ln != nil {
			ln.Close()
		}
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("BIND not sent")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled BIND: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled BIND did not return")
	}
}

func TestListenerClosePendingStream(t *testing.T) {
	for _, partialHeader := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial-header=%t", partialHeader), func(t *testing.T) {
			ready, done := make(chan struct{}), make(chan error, 1)
			proxy := scriptedPeer(t, func(c net.Conn) {
				var req relay.Request
				if _, err := req.ReadFrom(c); err != nil {
					done <- err
					return
				}
				addr := &relay.AddrFeature{}
				addr.ParseFrom("127.0.0.1:8080")
				resp := relay.Response{Version: relay.Version1, Status: relay.StatusOK, Features: []relay.Feature{addr}}
				resp.WriteTo(c)
				session, err := smux.Client(c, smux.DefaultConfig())
				if err != nil {
					done <- err
					return
				}
				defer session.Close()
				stream, err := session.OpenStream()
				if err != nil {
					done <- err
					return
				}
				defer stream.Close()
				if partialHeader {
					stream.Write([]byte{relay.Version1})
				} else {
					resp.WriteTo(stream)
				}
				close(ready)
				_, err = stream.Read(make([]byte, 1))
				done <- err
			})
			ln, _ := listenForTest(t, config(proxy))
			select {
			case <-ready:
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not start")
			}
			ln.Close() // No Accept: either header reading or handoff is pending.
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
						t.Errorf("Accept after close: %v", err)
					}
				}()
			}
			wg.Wait()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Close left pending stream usable")
				}
			case <-time.After(time.Second):
				t.Fatal("Close left pending stream/session blocked")
			}
		})
	}
}

func TestOfficialListenerConcurrentAcceptCancellation(t *testing.T) {
	proxy := freeAddr(t)
	kill := startOfficial(t, proxy, true)
	cfg := config(proxy)
	e := &events{failed: make(chan string, 32)}
	cfg.Logger = slog.New(e)
	ln, cancel := listenForTest(t, cfg)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Errorf("blocked Accept: %v", err)
			}
		}()
	}
	kill()
	event(t, e.failed) // The session has failed and background retry is pending.
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel during retry did not unblock all Accept calls")
	}
}
