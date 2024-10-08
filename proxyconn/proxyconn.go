// Copyright (C) 2024 Michael J. Fromberger. All Rights Reserved.

// Package proxyconn implements a "proxy connector" that bridges between HTTP
// CONNECT requests and a reverse proxy.
package proxyconn

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// A Bridge is an [http.Handler] that forwards requests to a reverse proxy.
//
// It handles plain HTTP requests by delegating directly to an underlying
// handler, and provides a minimal implementation of the HTTP CONNECT protocol
// that implements [net.Listener]. When a valid CONNECT request is received, it
// hijacks the connection and forwards it via the listener's Accept method.
type Bridge struct {
	// Addrs define the host[:port] combinations the Bridge will accept as
	// targets for a CONNECT request to be proxied. If none are defined, CONNECT
	// requests will be forwarded directly, or rejected, depending on the value
	// of the ForwardConnect option. If a port is omitted, ":443" is assumed.
	Addrs []string

	// Handler is the underlying handler to which plain HTTP requests are
	// delegated. If nil, requests other than CONNECT are rejected.
	Handler http.Handler

	// ForwardConnect, if true, causes the Bridge to directly forward CONNECT
	// requests that are not listed in Addrs to their specified targets, without
	// delivering them to the Accept method.  When false, CONNECT requests not
	// matching one of the Addrs are rejected.
	ForwardConnect bool

	// Logf, if non-nil, is used to write log messages.  If nil, logs are
	// discarded.
	Logf func(string, ...any)

	initOnce sync.Once
	queue    chan net.Conn // channels waiting to be Accepted
	stopped  chan struct{} // closed when the Bridge is closed

	httpProxyReject   expvar.Int // HTTP proxy requests rejected
	httpProxyDelegate expvar.Int // HTTP proxy requests delegated
	fwdConnReject     expvar.Int // unclaimed CONNECT rejected
	fwdConnError      expvar.Int // unclaimed CONNECT failed
	fwdConnSplice     expvar.Int // unclaimed CONNECT spliced
	proxyConnRequest  expvar.Int // matching CONNECT requests
	proxyConnError    expvar.Int // matching CONNECT failed
	proxyConnAccept   expvar.Int // matching CONNECT accepted
}

func (b *Bridge) init() {
	b.initOnce.Do(func() {
		b.stopped = make(chan struct{})
		b.queue = make(chan net.Conn)
	})
}

// Metrics returns a map of bridge metrics. The caller is responsible for
// exporting these metrics.
func (b *Bridge) Metrics() *expvar.Map {
	m := new(expvar.Map)
	m.Set("http_proxy_reject", &b.httpProxyReject)
	m.Set("http_proxy_delegate", &b.httpProxyDelegate)
	m.Set("fwd_conn_reject", &b.fwdConnReject)
	m.Set("fwd_conn_error", &b.fwdConnError)
	m.Set("fwd_conn_splice", &b.fwdConnSplice)
	m.Set("proxy_conn_request", &b.proxyConnRequest)
	m.Set("proxy_conn_error", &b.proxyConnError)
	m.Set("proxy_conn_accept", &b.proxyConnAccept)
	return m
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

// ServeHTTP implements the [http.Handler] interface. It handles CONNECT
// requests and forwards their connections (if accepted) to the Accept method;
// all other requests are delegated to the underlying handler, if one is
// defined.
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
		b.forwardConnect(w, r)
		return
	}
	b.proxyConnRequest.Add(1)

	conn, bw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		b.proxyConnError.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bw.Flush()
	// Hereafter, the server will no longer use or maintain conn, and we must
	// handle all writes and closes ourselves.

	if err := b.push(r.Context(), conn); err != nil {
		b.proxyConnError.Add(1)
		defer conn.Close()
		fmt.Fprintf(conn, "%s %d %s\r\n\r\n",
			r.Proto, http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		return
	}
	b.proxyConnAccept.Add(1)

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
		b.httpProxyReject.Add(1)
		b.logf("reject proxy request %v", r.URL)
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	b.httpProxyDelegate.Add(1)
	b.Handler.ServeHTTP(w, r)
}

// forwardConnect either forwards the specified request to the underlying
// handler, or reports an error.
func (b *Bridge) forwardConnect(w http.ResponseWriter, r *http.Request) {
	if !b.ForwardConnect {
		b.fwdConnReject.Add(1)
		b.logf("reject CONNECT for target %q", r.URL.Host)
		http.Error(w, fmt.Sprintf("target address %q not recognized", r.URL.Host), http.StatusForbidden)
		return
	}

	// Dial the remote server.
	rconn, err := net.Dial("tcp", r.URL.Host)
	if err != nil {
		b.fwdConnError.Add(1)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Hijack the caller's connection.
	cconn, bw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		b.fwdConnError.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bw.Flush()
	// Hereafter, the server will no longer use or maintain conn, and we must
	// handle all writes and closes ourselves.

	// Report that the connection is being transferred...
	fmt.Fprintf(cconn, "%s 200 OK\r\n\r\n", r.Proto)

	// Splice the connections together.
	b.fwdConnSplice.Add(1)
	go func() {
		io.Copy(cconn, rconn)
		cconn.(*net.TCPConn).CloseWrite()
	}()
	go func() {
		io.Copy(rconn, cconn)
		rconn.(*net.TCPConn).CloseWrite()
	}()
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

func (b *Bridge) logf(msg string, args ...any) {
	if b.Logf != nil {
		b.Logf(msg, args...)
	}
}

// addrStub implements the [net.Addr] interface for a fake address.
type addrStub string

func (a addrStub) Network() string { return "tcp" }
func (a addrStub) String() string  { return string(a) }
