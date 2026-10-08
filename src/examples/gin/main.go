// This independent consumer module keeps Gin out of the SDK's dependencies.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	rtcp "example.com/gostrtcpsdk/src"
	"github.com/gin-gonic/gin"
)

func main() {
	gin.SetMode(gin.ReleaseMode)
	cfg := rtcp.Config{Server: os.Getenv("GOST_SERVER"), Bind: os.Getenv("GOST_BIND")}
	if cfg.Server == "" || cfg.Bind == "" {
		slog.Error("set GOST_SERVER and GOST_BIND")
		os.Exit(1)
	}
	if user, password := os.Getenv("GOST_USER"), os.Getenv("GOST_PASSWORD"); user != "" || password != "" {
		cfg.User = url.UserPassword(user, password)
	}
	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := serve(stop, cfg, router(), slog.Default()); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.SetTrustedProxies(nil)
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong", "ip": c.ClientIP()})
	})
	r.POST("/echo", func(c *gin.Context) {
		var payload struct {
			Message string `json:"message"`
		}
		if err := c.ShouldBindJSON(&payload); err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.JSON(http.StatusOK, payload)
	})
	return r
}

// serve accepts any http.Handler, including a Gin engine or JmptJwxtAPI's
// httpapi.Server. Its HTTP limits and shutdown sequence match that application.
func serve(stop context.Context, cfg rtcp.Config, handler http.Handler, logger *slog.Logger) error {
	// Startup honors stop; after startup the tunnel lives through HTTP draining.
	tunnel, cancelTunnel := context.WithCancel(context.Background())
	defer cancelTunnel()
	stopStartup := context.AfterFunc(stop, cancelTunnel)
	listener, err := rtcp.Listen(tunnel, cfg)
	stopStartup()
	if err != nil {
		if stop.Err() != nil {
			return nil
		}
		return err
	}
	defer listener.Close()

	requests, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return requests },
	}
	defer server.Close()
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	logger.Info("service started", "address", listener.Addr().String())
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-stop.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		cancelRequests()
		server.Close()
	}
	<-finished
	return nil
}
