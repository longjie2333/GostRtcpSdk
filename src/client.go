// Copyright (c) 2016 ginuerzh. Derived from go-gost/x v0.16.0; see NOTICE.
// Package rtcp connects to an official gost Relay+TLS server and forwards
// incoming remote TCP streams to a local target. It contains no server.
package rtcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/relay"
	"github.com/xtaci/smux"
)

// Config describes the official gost endpoint and remote bind address.
// TLS=nil preserves gost's default InsecureSkipVerify=true. A non-nil TLS
// config is cloned; configure trusted roots/ServerName there when required.
// Treat Config and referenced values as immutable during Run.
type Config struct {
	Server string
	Bind   string
	User   *url.Userinfo
	TLS    *tls.Config
	Logger *slog.Logger
}

// Client owns one forwarding service. Construct with NewClient; do not copy it.
// Configuration is immutable; UpdateTarget may run concurrently with Run.
type Client struct {
	cfg     Config
	mu      sync.RWMutex
	target  string
	running bool
}

// NewClient validates server/target address syntax without network connections.
func NewClient(cfg Config, target string) (*Client, error) {
	if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg}
	if err := c.UpdateTarget(target); err != nil {
		return nil, err
	}
	return c, nil
}

// UpdateTarget atomically changes the default target for streams whose target
// has not yet been selected. Existing connections are unchanged. Validation is
// syntactic only: no DNS lookup or connection is attempted; failure keeps the
// previous target. A peer-provided target override still takes precedence.
func (c *Client) UpdateTarget(target string) error {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 || host == "" || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("invalid target: expected host and numeric port 1..65535")
	}
	c.mu.Lock()
	c.target = target
	c.mu.Unlock()
	return nil
}

func (c *Client) currentTarget() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.target
}

// Run blocks until cancellation and owns all session/forwarding resources.
// Only one Run may be active per Client. After it returns, Run may be called
// again and retains the latest target. BIND retries keep their existing policy.
func (c *Client) Run(ctx context.Context) error {
	if _, _, err := net.SplitHostPort(c.cfg.Server); err != nil {
		return err
	}
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("client is already running")
	}
	c.running = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()
	cfg := c.cfg
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	var delay time.Duration
	for ctx.Err() == nil {
		session, bound, err := bind(ctx, cfg)
		if err == nil {
			log.Info("remote TCP listening", "bind", bound, "target", c.currentTarget())
			stop := context.AfterFunc(ctx, func() { session.Close() })
			for {
				stream, e := session.AcceptStream()
				var peer, override string
				if e == nil {
					peer, override, e = readResponse(stream)
				} // each stream has its own Relay header
				var peerAddr *net.TCPAddr
				if e == nil {
					peerAddr, e = net.ResolveTCPAddr("tcp", peer)
				}
				if e != nil {
					if stream != nil {
						stream.Close()
					}
					err = e
					delay = 0
					break
				}
				delay = 0
				dst := c.currentTarget()
				if override != "" {
					dst = override
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer stream.Close()
					out, e := (&net.Dialer{Timeout: 15 * time.Second, Resolver: &net.Resolver{PreferGo: true}}).DialContext(ctx, "tcp", dst)
					if e != nil {
						log.Warn("target dial failed", "target", dst, "error", e)
						return
					}
					defer out.Close()
					log.Debug("forward start", "peer", peerAddr, "target", dst)
					// smux v1.5.31 Stream implements net.Conn, but has no CloseWrite.
					// No wrapper is needed: Run exposes no stream/deadline API.
					pipe(ctx, stream, out)
					log.Debug("forward end", "peer", peerAddr, "target", dst)
				}()
			}
			stop()
			session.Close()
		}
		if ctx.Err() != nil {
			break
		}
		delay = min(max(time.Second, 2*delay), 5*time.Second)
		log.Warn("remote TCP retry", "error", err, "delay", delay)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	return ctx.Err()
}

// One attempt owns cleanup on failure; Run owns the returned session.
func bind(ctx context.Context, cfg Config) (session *smux.Session, bound string, err error) {
	address := cfg.Bind
	if a, e := net.ResolveTCPAddr("tcp", address); e == nil {
		address = a.String()
	}
	// Preserve x v0.16.0's :8080 -> 0.0.0.0::8080 defect; use an explicit host.
	if address == "" {
		address = "0.0.0.0:0"
	} else if h, _, e := net.SplitHostPort(address); e == nil && h == "" {
		address = net.JoinHostPort("0.0.0.0", address)
	}
	af := &relay.AddrFeature{}
	af.ParseFrom(address)
	tc := cfg.TLS
	if tc == nil {
		tc = &tls.Config{InsecureSkipVerify: true}
	} else {
		tc = tc.Clone()
	}
	if tc.ServerName == "" {
		tc.ServerName, _, _ = net.SplitHostPort(cfg.Server)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	d := tls.Dialer{NetDialer: &net.Dialer{Resolver: &net.Resolver{PreferGo: true}}, Config: tc}
	conn, err := d.DialContext(dialCtx, "tcp", cfg.Server)
	cancel()
	if err != nil {
		return nil, "", err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	req := relay.Request{Version: relay.Version1, Cmd: relay.CmdBind}
	if cfg.User != nil {
		pass, _ := cfg.User.Password()
		req.Features = append(req.Features, &relay.UserAuthFeature{Username: cfg.User.Username(), Password: pass})
	}
	req.Features = append(req.Features, &relay.NetworkFeature{Network: relay.NetworkTCP}, af)
	if _, err = req.WriteTo(conn); err != nil {
		return nil, "", err
	}
	first, last, err := readResponse(conn)
	if err != nil {
		return nil, "", err
	}
	if last != "" {
		first = last
	} // initial bind reply takes its last AddrFeature
	addr, err := net.ResolveTCPAddr("tcp", first)
	if err != nil {
		return nil, "", err
	}
	session, err = smux.Server(conn, smux.DefaultConfig())
	if err != nil {
		return nil, "", err
	}
	if err = ctx.Err(); err != nil {
		session.Close()
		return nil, "", err
	}
	return session, addr.String(), nil
}

// Both reply sites share framing/status parsing; each caller owns its address
// semantics. The optional second address in a peer reply overrides the target.
func readResponse(r io.Reader) (first, other string, err error) {
	var resp relay.Response
	if _, err = resp.ReadFrom(r); err != nil {
		return
	}
	if resp.Status != relay.StatusOK {
		return "", "", fmt.Errorf("relay: status 0x%02x", resp.Status)
	}
	for _, f := range resp.Features {
		if af, ok := f.(*relay.AddrFeature); ok {
			addr := net.JoinHostPort(af.Host, strconv.Itoa(int(af.Port)))
			if first == "" {
				first = addr
			} else {
				other = addr
			}
		}
	}
	return
}
