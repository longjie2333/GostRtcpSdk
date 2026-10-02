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

// Run forwards remote TCP streams until ctx is cancelled. It owns the TLS
// connection, smux session and target sockets; cancellation closes them and
// waits for forwarding goroutines. BIND failures back off 1,2,4,5 seconds;
// session/peer-header failures discard the session and retry after 1 second.
func Run(ctx context.Context, cfg Config, target string) error {
	for _, addr := range []string{cfg.Server, target} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return err
		}
	}
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
			log.Info("remote TCP listening", "bind", bound, "target", target)
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
				dst := target
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
