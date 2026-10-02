package rtcp

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// A scripted net.Conn exposes boundary conditions real TCP rarely returns.
type boundaryConn struct {
	mu          sync.Mutex
	read        []byte
	eofWithData bool
	shortWrite  bool
	writes      bytes.Buffer
}

func (c *boundaryConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.read) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.read)
	c.read = c.read[n:]
	if c.eofWithData {
		return n, io.EOF
	}
	return n, nil
}
func (c *boundaryConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shortWrite && len(p) > 2 {
		p = p[:2]
	}
	return c.writes.Write(p)
}
func (c *boundaryConn) Close() error                     { return nil }
func (c *boundaryConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *boundaryConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *boundaryConn) SetDeadline(time.Time) error      { return nil }
func (c *boundaryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *boundaryConn) SetWriteDeadline(time.Time) error { return nil }

func TestPipeReadDataAndEOFDropsTail(t *testing.T) {
	a := &boundaryConn{read: []byte("tail"), eofWithData: true}
	b := &boundaryConn{}
	if err := pipe(context.Background(), a, b); err != nil {
		t.Fatal(err)
	}
	if b.writes.Len() != 0 {
		t.Fatal("v0.16.0 Read(n, EOF) behavior changed")
	}
}

func TestPipeShortWriteIsNotRetried(t *testing.T) {
	a := &boundaryConn{read: []byte("abcdef")}
	b := &boundaryConn{shortWrite: true}
	if err := pipe(context.Background(), a, b); err != nil {
		t.Fatal(err)
	}
	if b.writes.String() != "ab" {
		t.Fatalf("got %q", b.writes.String())
	}
}

func TestPipeCancellationUnblocksBothHalves(t *testing.T) {
	a, pa := net.Pipe()
	defer pa.Close()
	b, pb := net.Pipe()
	defer pb.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pipe(ctx, a, b) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Pipe returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked goroutines")
	}
}

// Isolate copy-buffer allocation cost; this is not a network throughput test.
func BenchmarkPipe(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 4096)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		a := &boundaryConn{read: payload}
		z := &boundaryConn{}
		if err := pipe(context.Background(), a, z); err != nil {
			b.Fatal(err)
		}
	}
}
