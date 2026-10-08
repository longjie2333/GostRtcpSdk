package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	rtcp "example.com/gostrtcpsdk/src"
	"github.com/gin-gonic/gin"
)

func TestGinThroughOfficialGOST(t *testing.T) {
	binary := os.Getenv("GOST_V3_BINARY")
	if binary == "" {
		t.Skip("set GOST_V3_BINARY to official gost v3.3.0")
	}
	// These temporary sockets reserve addresses only for the two PUBLIC test
	// endpoints. The business application never calls net.Listen.
	var addresses []string
	for i := 0; i < 2; i++ {
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addresses = append(addresses, probe.Addr().String())
		probe.Close()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-L", fmt.Sprintf("relay+tls://user:password@%s?bind=true", addresses[0]))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addresses[0], 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("official GOST did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	gin.SetMode(gin.ReleaseMode)
	r := router()
	started, release := make(chan struct{}), make(chan struct{})
	r.GET("/drain", func(c *gin.Context) {
		close(started)
		select {
		case <-release:
			c.String(http.StatusOK, "completed")
		case <-c.Request.Context().Done():
		}
	})
	stop, cancelServe := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(stop, rtcp.Config{Server: addresses[0], Bind: addresses[1], User: url.UserPassword("user", "password")}, r, slog.Default())
	}()
	t.Cleanup(cancelServe)
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	base := "http://" + addresses[1]
	for {
		resp, err := client.Get(base + "/ping")
		if err == nil {
			var body struct{ Message, IP string }
			err = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || body.Message != "pong" || body.IP != "127.0.0.1" {
				t.Fatalf("Gin ping: %+v status=%d error=%v", body, resp.StatusCode, err)
			}
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve failed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := client.Post(base+"/echo", "application/json", strings.NewReader(`{"message":"through the tunnel"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != `{"message":"through the tunnel"}` {
		t.Fatalf("Gin POST: %s %v", body, err)
	}
	drained := make(chan error, 1)
	go func() {
		resp, err := client.Get(base + "/drain")
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			err = readErr
			if string(body) != "completed" {
				err = fmt.Errorf("request interrupted: %q", body)
			}
		}
		drained <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Gin handler did not start")
	}
	cancelServe()
	// The generic listener's exact Close/drain ordering is separately tested in
	// the SDK. Here exercise the consuming application's shutdown context wiring.
	close(release)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Gin server did not shut down")
	}
}
