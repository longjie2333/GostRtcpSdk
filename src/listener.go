package rtcp

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

// Listen binds a TCP address on the official GOST server and returns a listener
// for application connections. It opens no local listening socket. The first
// TLS/BIND attempt is synchronous; errors are returned to the caller. Subsequent
// session failures reconnect in the background with Run's retry policy.
//
// Addr reports the latest successful remote binding (port 0 requests allocation).
// Accepted connections report that binding as LocalAddr and the visitor address
// as RemoteAddr. Relay headers are consumed; target overrides are ignored.
//
// Close stops Accept but leaves accepted connections usable, allowing HTTP
// Shutdown to drain requests. Each accepted connection MUST be closed by its
// owner. The remote port/session is released after the last connection closes.
// Canceling ctx instead closes the listener AND all connections immediately.
// Keep ctx alive until application shutdown finishes if graceful draining matters.
func Listen(ctx context.Context, cfg Config) (net.Listener, error) {
	if _, _, err := net.SplitHostPort(cfg.Server); err != nil {
		return nil, err
	}
	session, bound, err := bind(ctx, cfg)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	}
	addr, err := net.ResolveTCPAddr("tcp", bound)
	if err != nil {
		session.Close()
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	acceptCtx, cancel := context.WithCancel(ctx)
	l := &listener{
		cfg: cfg, ctx: acceptCtx, cancel: cancel,
		incoming: make(chan net.Conn), session: session, addr: addr,
	}
	// The cancellation callback may run immediately, so publish stop under mu.
	l.mu.Lock()
	l.stop = context.AfterFunc(ctx, func() {
		l.Close()
		l.mu.Lock()
		if l.session != nil {
			l.session.Close()
		}
		l.mu.Unlock()
	})
	l.mu.Unlock()
	cfg.Logger.Info("remote TCP listening", "bind", bound)
	go l.run()
	return l, nil
}

type listener struct {
	cfg      Config
	ctx      context.Context
	cancel   context.CancelFunc
	incoming chan net.Conn

	mu      sync.Mutex
	session *smux.Session
	addr    *net.TCPAddr
	active  int // Includes the stream being handed to Accept; only current session.
	closed  bool
	stop    func() bool
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.incoming:
		if l.ctx.Err() == nil {
			return conn, nil
		}
		conn.Close()
	case <-l.ctx.Done():
	}
	return nil, net.ErrClosed
}

func (l *listener) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Do not expose mutable address storage shared by other connections.
	addr := *l.addr
	addr.IP = append(net.IP(nil), addr.IP...)
	return &addr
}

func (l *listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.cancel()
	if l.active == 0 {
		if l.session != nil {
			l.session.Close()
		}
		l.stop()
	}
	return nil
}

func (l *listener) run() {
	session := l.session
	addr := l.addr
	var delay time.Duration
	for l.ctx.Err() == nil {
		stream, err := session.AcceptStream()
		var peer string
		if err == nil {
			stop := context.AfterFunc(l.ctx, func() { stream.Close() })
			peer, _, err = readResponse(stream)
			stop()
		}
		var remote *net.TCPAddr
		if err == nil {
			remote, err = net.ResolveTCPAddr("tcp", peer)
		}
		if err == nil {
			conn := &businessConn{Conn: stream, listener: l, session: session, local: addr, remote: remote}
			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				stream.Close()
				return
			}
			l.active++
			l.mu.Unlock()
			select {
			case l.incoming <- conn:
			case <-l.ctx.Done():
				conn.Close()
				return
			}
			delay = 0
			continue
		}
		if stream != nil {
			stream.Close()
		}
		if l.ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		session.Close()
		l.session = nil
		l.active = 0 // Connections from this failed session cannot be reused.
		if l.closed {
			l.stop()
		}
		l.mu.Unlock()
		for l.ctx.Err() == nil {
			delay = min(max(time.Second, 2*delay), 5*time.Second)
			l.cfg.Logger.Warn("remote TCP retry", "error", err, "delay", delay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-l.ctx.Done():
				timer.Stop()
				return
			}
			var bound string
			session, bound, err = bind(l.ctx, l.cfg)
			if err != nil {
				continue
			}
			addr, err = net.ResolveTCPAddr("tcp", bound)
			if err != nil {
				session.Close()
				continue
			}
			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				session.Close()
				return
			}
			l.session, l.addr = session, addr
			l.mu.Unlock()
			l.cfg.Logger.Info("remote TCP listening", "bind", bound)
			delay = 0
			break
		}
	}
}

// Only address metadata and ownership differ from smux.Stream. Deadlines remain
// stream-local, never applied to the shared TLS connection.
type businessConn struct {
	net.Conn
	listener      *listener
	session       *smux.Session
	local, remote *net.TCPAddr
	once          sync.Once
	err           error
}

func (c *businessConn) LocalAddr() net.Addr {
	addr := *c.local
	addr.IP = append(net.IP(nil), addr.IP...)
	return &addr
}

func (c *businessConn) RemoteAddr() net.Addr {
	addr := *c.remote
	addr.IP = append(net.IP(nil), addr.IP...)
	return &addr
}

func (c *businessConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		l := c.listener
		l.mu.Lock()
		defer l.mu.Unlock()
		if c.session == l.session {
			l.active--
			if l.closed && l.active == 0 {
				l.session.Close()
				l.stop()
			}
		}
	})
	return c.err
}
