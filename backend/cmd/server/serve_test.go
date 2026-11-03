package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestNewServerTimeouts(t *testing.T) {
	h := http.NewServeMux()
	srv := newServer("127.0.0.1:0", h)
	if srv.Addr != "127.0.0.1:0" || srv.Handler != h {
		t.Fatalf("addr/handler not set: %+v", srv)
	}
	if srv.ReadHeaderTimeout != 10*time.Second || srv.ReadTimeout != 30*time.Second ||
		srv.IdleTimeout != 120*time.Second || srv.WriteTimeout != 3*time.Minute {
		t.Fatalf("timeouts: rh=%v r=%v i=%v w=%v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout, srv.WriteTimeout)
	}
}

func TestServeShutsDownGracefullyOnContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	release := make(chan struct{})
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "finished")
	})
	srv := newServer(ln.Addr().String(), h)
	var logBuf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logBuf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, ln, lg) }()

	// Start an in-flight request, then ask for shutdown while it runs.
	respCh := make(chan string, 1)
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respCh <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		respCh <- string(b)
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("serve returned before serving: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}
	cancel()

	select {
	case err := <-done:
		t.Fatalf("serve returned before in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if got := <-respCh; got != "finished" {
		t.Fatalf("in-flight request was not drained: %q", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("shutting down")) {
		t.Errorf("shutdown not logged: %s", logBuf.String())
	}
}

func TestServeReturnsListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // Serve on a closed listener fails immediately.
	srv := newServer(ln.Addr().String(), http.NewServeMux())
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := serve(context.Background(), srv, ln, lg); err == nil {
		t.Fatal("expected error from closed listener")
	}
}
