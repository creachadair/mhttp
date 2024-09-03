// Copyright (C) 2024 Michael J. Fromberger. All Rights Reserved.

package proxyconn_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/creachadair/mds/mtest"
	"github.com/creachadair/mhttp/proxyconn"
	"github.com/creachadair/tlsutil"
	gocmp "github.com/google/go-cmp/cmp"
)

func TestBridge(t *testing.T) {
	var reqs []string
	var ndirect int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, fmt.Sprintf("%s %s", r.Method, r.URL))
		fmt.Fprintln(w, "ok, got it")
	})

	// Set up a fake "backend" to make sure connections not claimed by the proxy
	// are correctly redirected.
	alt := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs = append(reqs, fmt.Sprintf("remote %s %s", r.Method, r.URL))
		fmt.Fprintln(w, "do you come from a land down under?")
	}))
	alt.StartTLS()
	defer alt.Close()

	b := &proxyconn.Bridge{
		Addrs: []string{"fuzzbucket", "beeblebrox:443"},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ndirect++
			handler(w, r)
		}),
	}

	// Serve the proxy bridge to callers.
	hs := httptest.NewServer(b)
	defer hs.Close()
	hsURL, err := url.Parse(hs.URL)
	if err != nil {
		t.Fatalf("Parse %q: %v", hs.URL, err)
	}

	// Start a separate server to handler requests forwarded by the proxy.
	// This server "listens" on the proxy and does not use the network.
	cert, err := tlsutil.NewSigningCert(&x509.Certificate{
		DNSNames: []string{"fuzzbucket", "beeblebrox"},
	}, 10*time.Minute)
	if err != nil {
		t.Fatalf("Create certificate: %v", err)
	}
	tlsCert, err := cert.TLSCertificate()
	if err != nil {
		t.Fatalf("Export TLS cert: %v", err)
	}
	pserv := &http.Server{
		Handler: handler,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{tlsCert},
		},
	}
	pdone := make(chan struct{})
	go func() {
		defer close(pdone)
		if err := pserv.ServeTLS(b, "", ""); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Proxy server error: %v", err)
		}
	}()

	if _, err := http.Get(hs.URL + "/direct"); err != nil {
		t.Errorf("Get plain: unexpected error: %v", err)
	}

	// Set up our own transport with knowledge of the server's custom cert.
	certPool := x509.NewCertPool()
	certPool.AppendCertsFromPEM(cert.CertPEM())
	tsp := &http.Transport{
		Proxy:           http.ProxyURL(hsURL),
		TLSClientConfig: &tls.Config{RootCAs: certPool},
	}
	cli := &http.Client{Transport: tsp}

	if _, err := cli.Get("https://fuzzbucket/proxied"); err != nil {
		t.Errorf("Get proxied: unexpected error: %v", err)
	}
	if _, err := cli.Head("https://beeblebrox:443/proxied"); err != nil {
		t.Errorf("Head proxied: unexpected error: %v", err)
	}
	if _, err := cli.Post("https://fuzzbucket:443/proxied", "text/plain", strings.NewReader("hi")); err != nil {
		t.Errorf("Post proxied: unexpected error: %v", err)
	}
	if rsp, err := cli.Get("https://nonesuch/bad"); err == nil {
		t.Errorf("Get nonesuch: got %+v, want error", rsp)
	}

	// At this point the bridge does not forward connections for targets not in
	// its addrs list. Make sure that is enforced.
	t.Run("NoForward", func(t *testing.T) {
		if rsp, err := cli.Get(alt.URL + "/reject"); err == nil {
			t.Errorf("Get(%s/reject): unexpected success", alt.URL)
			if got, want := rsp.StatusCode, http.StatusForbidden; got != want {
				t.Errorf("Get(%s/reject): got status %d, want %d", alt.URL, got, want)
			}
		}
	})

	// Now set ForwardConnect and verify that the bridge properly forwards to
	// the alt server.  For this, disable cert verification because we can't get
	// the test server's cert and that doesn't matter for the plumbing we're
	// testing here.
	t.Run("Forward", func(t *testing.T) {
		mtest.Swap(t, &b.ForwardConnect, true)
		mtest.Swap(t, &tsp.TLSClientConfig.InsecureSkipVerify, true)

		if _, err := cli.Get(alt.URL + "/ok"); err != nil {
			t.Errorf("Get %s/ok: unexpected error: %v", alt.URL, err)
		}
	})

	pserv.Shutdown(context.Background())
	<-pdone
	hs.Close()

	// Make sure the plumbing worked.
	if diff := gocmp.Diff(reqs, []string{
		"GET /direct", "GET /proxied", "HEAD /proxied", "POST /proxied", "remote GET /ok",
	}); diff != "" {
		t.Errorf("Requests (-got, +want):\n%s", diff)
	}
	if ndirect != 1 {
		t.Errorf("Got %d direct requests, want 1", ndirect)
	}

}
