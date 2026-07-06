package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// P2-1: graceful shutdown must DRAIN an in-flight request rather than hard-kill it.
// We start serveGraceful, fire a request whose handler is mid-flight (blocked on a
// gate), then cancel the context (the signal seam). The in-flight request MUST
// still complete with its full body — proving Shutdown drained it instead of
// truncating the relay — and serveGraceful must return nil (a clean shutdown).
func TestServeGracefulDrainsInFlightRequest(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()

	const body = "in-flight-response-body-not-truncated"

	handlerEntered := make(chan struct{}) // closed once the handler is running
	release := make(chan struct{})        // the test releases the handler to finish
	var once sync.Once

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(handlerEntered) })
		<-release // simulate a slow upstream relay in progress
		fmt.Fprint(w, body)
	})

	ctx, cancel := context.WithCancel(context.Background())
	// stopSignals is the signal-release seam; a no-op is fine for the test.
	var stopCalled bool
	stopSignals := func() { stopCalled = true }

	serveDone := make(chan error, 1)
	go func() { serveDone <- serveGraceful(ctx, l, h, stopSignals) }()

	// Fire the request in a goroutine; it will block inside the handler.
	type resp struct {
		body string
		err  error
	}
	respCh := make(chan resp, 1)
	go func() {
		c := &http.Client{Timeout: 10 * time.Second}
		r, err := c.Get("http://" + addr + "/")
		if err != nil {
			respCh <- resp{err: err}
			return
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		respCh <- resp{body: string(b), err: err}
	}()

	// Wait until the handler is actually running (request is in-flight).
	select {
	case <-handlerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never entered — request did not reach the server")
	}

	// Now trigger shutdown WHILE the request is in-flight.
	cancel()

	// Give Shutdown a moment to begin draining, then release the handler so it can
	// finish. A truncating (hard-kill) shutdown would have already killed the conn.
	time.Sleep(100 * time.Millisecond)
	close(release)

	// The in-flight request must complete with its FULL body.
	select {
	case r := <-respCh:
		if r.err != nil {
			t.Fatalf("in-flight request was not drained cleanly: %v", r.err)
		}
		if r.body != body {
			t.Fatalf("in-flight response truncated: got %q, want %q", r.body, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed (shutdown may have hard-killed it)")
	}

	// serveGraceful must return nil for a signal-driven shutdown.
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveGraceful returned an error on clean shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveGraceful did not return after shutdown")
	}

	if !stopCalled {
		t.Fatal("serveGraceful must release signal handling (stopSignals) on shutdown so a second Ctrl-C hard-kills")
	}
}

// A NEW request that arrives AFTER shutdown has begun must be refused (the
// listener is closed) — Shutdown stops accepting new connections. This proves we
// don't keep taking work while draining.
func TestServeGracefulRefusesNewRequestsAfterShutdown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveGraceful(ctx, l, h, func() {}) }()

	// Confirm it serves before shutdown.
	c := &http.Client{Timeout: 5 * time.Second}
	if r, err := c.Get("http://" + addr + "/"); err != nil {
		t.Fatalf("pre-shutdown request failed: %v", err)
	} else {
		r.Body.Close()
	}

	cancel() // begin shutdown

	// After shutdown returns, the listener is closed → new connections are refused.
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveGraceful returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveGraceful did not return")
	}

	if r, err := c.Get("http://" + addr + "/"); err == nil {
		r.Body.Close()
		t.Fatal("a request after shutdown must be refused (listener closed)")
	}
}

// If the listener dies on its own (no shutdown signal), serveGraceful surfaces the
// Serve error rather than swallowing it — a broken listen must not look like a
// clean exit.
func TestServeGracefulSurfacesServeError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx := context.Background() // never cancelled — no shutdown path
	done := make(chan error, 1)
	go func() {
		done <- serveGraceful(ctx, l, http.NewServeMux(), func() {})
	}()

	// Close the listener out from under Serve: Serve returns a non-ErrServerClosed
	// error, which serveGraceful must surface (not treat as a clean shutdown).
	time.Sleep(50 * time.Millisecond)
	_ = l.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serveGraceful must surface a Serve error when the listener dies unexpectedly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveGraceful did not return after the listener closed")
	}
}
