package rtcp

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func taggedTarget(t *testing.T, tag byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); c.Write([]byte{tag}); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func taggedVisitor(t *testing.T, bound string, tag byte) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", bound, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != tag {
		t.Fatalf("target tag: got %q, want %q, error %v", b, tag, err)
	}
	return c
}

func TestOfficialHotUpdate(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	a, b := taggedTarget(t, 'A'), taggedTarget(t, 'B')
	client, events, _ := startClient(t, config(proxy), a)
	bound := event(t, events.bound)
	old := taggedVisitor(t, bound, 'A')
	if err := client.UpdateTarget(b); err != nil {
		t.Fatal(err)
	}
	taggedVisitor(t, bound, 'B').Close()
	if err := client.UpdateTarget("bad-target"); err == nil {
		t.Fatal("invalid update accepted")
	}
	taggedVisitor(t, bound, 'B').Close()
	if _, err := old.Write([]byte("old stream")); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len("old stream"))
	if _, err := io.ReadFull(old, out); err != nil || string(out) != "old stream" {
		t.Fatalf("old connection interrupted: %q %v", out, err)
	}
	select {
	case addr := <-events.bound:
		t.Fatalf("update rebound the listener: %s", addr)
	default:
	}
}

func TestOfficialUpdatedTargetAfterReconnect(t *testing.T) {
	proxy := freeAddr(t)
	kill := startOfficial(t, proxy, true)
	cfg := config(proxy)
	cfg.Bind = freeAddr(t)
	client, events, _ := startClient(t, cfg, taggedTarget(t, 'A'))
	bound := event(t, events.bound)
	kill()
	if err := client.UpdateTarget(taggedTarget(t, 'B')); err != nil {
		t.Fatal(err)
	}
	startOfficial(t, proxy, true)
	if rebound := event(t, events.bound); rebound != bound {
		t.Fatal("unexpected bind address")
	}
	taggedVisitor(t, bound, 'B')
}

func TestUpdateTargetValidation(t *testing.T) {
	client, err := NewClient(config("127.0.0.1:1080"), "localhost:80")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", ":80", "localhost", "localhost:", "localhost:0", "localhost:65536", "localhost:-1", "localhost:http", "bad host:80", "http://localhost:80"} {
		if err := client.UpdateTarget(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
		if client.currentTarget() != "localhost:80" {
			t.Fatal("invalid update changed target")
		}
		if _, err := NewClient(config("127.0.0.1:1080"), bad); err == nil {
			t.Errorf("constructor accepted %q", bad)
		}
	}
	for _, good := range []string{"127.0.0.1:1", "[::1]:65535", "localhost:8080", "unresolved.invalid:80"} {
		if err := client.UpdateTarget(good); err != nil {
			t.Errorf("valid syntax %q: %v", good, err)
		}
	}
}

func TestOfficialConcurrentUpdates(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	a, b := startEcho(t).Addr().String(), startEcho(t).Addr().String()
	client, events, _ := startClient(t, config(proxy), a)
	bound := event(t, events.bound)
	var writers, readers sync.WaitGroup
	stopUpdates := make(chan struct{})
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for n := 0; ; n++ {
				select {
				case <-stopUpdates:
					return
				default:
				}
				if err := client.UpdateTarget([]string{a, b}[n%2]); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if err := checkEcho(bound, []byte("live updates")); err != nil {
				t.Error(err)
			}
		}()
	}
	readers.Wait()
	close(stopUpdates)
	writers.Wait()
	if err := client.UpdateTarget(b); err != nil {
		t.Fatal(err)
	}
	if client.currentTarget() != b {
		t.Fatal("last update was lost")
	}
}

func TestClientRunLifecycle(t *testing.T) {
	proxy := freeAddr(t)
	startOfficial(t, proxy, true)
	client, events, stop := startClient(t, config(proxy), startEcho(t).Addr().String())
	event(t, events.bound)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Run(ctx); err == nil || err.Error() != "client is already running" {
		t.Fatalf("concurrent Run: %v", err)
	}
	stop()
	if err := client.UpdateTarget("localhost:8081"); err != nil {
		t.Fatal(err)
	}
	if err := client.Run(ctx); err != context.Canceled {
		t.Fatalf("restart: %v", err)
	}
	if client.currentTarget() != "localhost:8081" {
		t.Fatal("restart reset target")
	}
}
