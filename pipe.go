// Copyright (c) 2016 ginuerzh. Derived from x/internal/net/pipe.go v0.16.0.
package rtcp

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

var buffers = sync.Pool{New: func() any { return make([]byte, 32*1024) }}

// pipe preserves v3.3.0's data path, including its edge cases: Read(n>0,EOF)
// drops those bytes and short successful writes are not retried. See report.
// TCP destinations get CloseWrite and a 10s read deadline; mux destinations
// lack CloseWrite and close the stream completely. Both halves are awaited.
func pipe(ctx context.Context, a, b net.Conn) error {
	errs := make(chan error, 2)
	for _, pair := range [][2]net.Conn{{a, b}, {b, a}} {
		go func(src, dst net.Conn) {
			var err error
			defer func() { errs <- err }()
			defer func() {
				if c, ok := src.(interface{ CloseRead() error }); ok {
					c.CloseRead()
				}
				if c, ok := dst.(interface{ CloseWrite() error }); ok {
					c.CloseWrite()
					dst.SetReadDeadline(time.Now().Add(10 * time.Second))
				} else {
					dst.Close()
				}
			}()
			buf := buffers.Get().([]byte)
			defer buffers.Put(buf)
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				var n int
				n, err = src.Read(buf)
				if err != nil {
					if err == io.EOF {
						err = nil
					}
					break
				}
				_, err = dst.Write(buf[:n])
				if err != nil {
					break
				}
			}
		}(pair[0], pair[1])
	}
	var first error
	stop := context.AfterFunc(ctx, func() { a.Close(); b.Close() })
	defer stop()
	for i := 0; i < 2; i++ {
		if err := <-errs; first == nil && err != nil {
			first = err
		}
	}
	if first != nil {
		return first
	}
	return ctx.Err()
}
