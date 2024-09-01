// Copyright (C) 2024 Michael J. Fromberger. All Rights Reserved.

// Package proxyconn implements a "proxy connector" that bridges between HTTP
// CONNECT requests and a reverse proxy.
package proxyconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// A Bridge is an [net/http.Handler] that forwards requests to a reverse proxy.
//
// It handles plain HTTP requests by delegated directly to an underlying
// handler, and provides a minimal implementation of the HTTP CONNECT protocol
// that implements [net.Listener]. When a valid CONNECT request is received, it
// hijacks the connection and forwards it via the listener's Accept method.
type Bridge struct {
	// Addrs define the host[:port] combinations the Connector will accept as
	// targets for a CONNECT request. If none are defined, CONNECT requests will
	// be rejected. If a port is omitted, ":443" is assumed.
	Addrs []string

	// Handler is the underlying handler to which plain HTTP requests are
	// delegated. If nil, requests other than CONNECT are rejected.
	Handler http.Handler

	initOnce sync.Once
	queue    chan net.Conn // channels waiting to be Accepted
	stopped  chan struct{} // closed when the Connector is closed
}

func (b *Bridge) init() {
	b.initOnce.Do(func() {
		b.stopped = make(chan struct{})
		b.queue = make(chan net.Conn)
	})
}

// Accept implements part of [net.Listener]. It blocks until either a
// connection is available, or b is closed. If a connection is not available
// before b closes, it reports [net.ErrClosed].
func (b *Bridge) Accept() (net.Conn, error) {
	b.init()
	select {
	case <-b.stopped:
		// fall through
	case conn, ok := <-b.queue:
		if ok {
			return conn, nil
		}
	}
	return nil, net.ErrClosed
}

// Close implements part of [net.Listener]. It is safe to call Close multiple
// times, but it must not be called concurrently from multiple goroutines.
func (b *Bridge) Close() error {
	b.init()
	select {
	case <-b.stopped:
		return net.ErrClosed
	default:
		close(b.stopped)
		return nil
	}
}

// Addr implements part of [net.Listener].
func (b *Bridge) Addr() net.Addr {
	b.init()
	if len(b.Addrs) == 0 {
		return addrStub("<invalid>")
	}
	return addrStub(b.Addrs[0])
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.init()

	// This endpoint allows only "CONNECT" requests; forward everything else.
	if r.Method != http.MethodConnect {
		b.delegateHTTP(w, r)
		return
	}

	// The CONNECT URL has a restricted form: host:port only.
	if r.URL.RawQuery != "" || r.URL.Fragment != "" || r.URL.Path != "" {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	if !b.hostMatchesTarget(r.URL.Host) {
		http.Error(w, fmt.Sprintf("target address %q not recognized", r.URL.Host), http.StatusForbidden)
		return
	}

	conn, bw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bw.Flush()
	// Hereafter, the server will no longer maintain conn, and we must maintain
	// and close it ourselves.

	if err := b.push(r.Context(), conn); err != nil {
		defer conn.Close()
		fmt.Fprintf(conn, "%s %d %s\r\n\r\n",
			r.Proto, http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		return
	}

	// Report success to the caller, then no more.
	fmt.Fprintf(conn, "%s 200 OK\r\n\r\n", r.Proto)
}

// push blocks until conn is delivered to a caller of Accept.  If that does not
// occur before ctx ends or b is closed, it reports an error.
func (b *Bridge) push(ctx context.Context, conn net.Conn) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.stopped:
		return errors.New("connection unavailable")
	case b.queue <- conn:
		return nil
	}
}

// delegateHTTP forwards the specified request to the underlying handler, or
// reports an error.
func (b *Bridge) delegateHTTP(w http.ResponseWriter, r *http.Request) {
	if b.Handler == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	b.Handler.ServeHTTP(w, r)
}

// hostMatchesTarget reports whether host matches any of the designated
// connection targets.
func (b *Bridge) hostMatchesTarget(host string) bool {
	for _, t := range b.Addrs {
		if host == t {
			return true
		} else if !strings.Contains(t, ":") && host == t+":443" {
			return true
		}
	}
	return false
}

// addrStub implements the [net.Addr] interface for a fake address.
type addrStub string

func (a addrStub) Network() string { return "tcp" }
func (a addrStub) String() string  { return string(a) }
