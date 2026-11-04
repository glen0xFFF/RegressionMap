package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const shutdownTimeout = 10 * time.Second

// drainTimeout is how long the process waits, after the HTTP shutdown, for
// running analyses to end and for completion notifications to be sent.
const drainTimeout = 25 * time.Second

// drainer is the part of *api.Server that a shutdown waits for.
type drainer interface {
	Drain(ctx context.Context) error
}

// serveAndDrain runs serve and then drains d: running analyses end as
// error, which starts their notifications, and the process waits up to
// drainTimeout for those and for every notification still being sent.
func serveAndDrain(ctx context.Context, srv *http.Server, ln net.Listener, lg *slog.Logger, d drainer) error {
	err := serve(ctx, srv, ln, lg)
	drain(d, lg, drainTimeout)
	return err
}

// drain calls d.Drain with a budget of timeout. If the budget runs out, the
// warning carries Drain's error, which counts the analysis runs still
// stopping and the notifications still being sent; such a notification is
// reported as failed by the next process.
func drain(d drainer, lg *slog.Logger, timeout time.Duration) {
	lg.Info("draining", "timeout", timeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := d.Drain(ctx); err != nil {
		lg.Warn("drain incomplete", "error", err.Error(), "timeout", timeout.String())
		return
	}
	lg.Info("drained")
}

// newServer sets the server timeouts. WriteTimeout is long because a
// verification request runs a test command synchronously (up to the command
// timeout) before the response is written.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      3 * time.Minute,
	}
}

// serve runs srv on ln until ctx is cancelled, then shuts down gracefully,
// waiting up to shutdownTimeout for in-flight requests.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, lg *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	lg.Info("shutting down", "timeout", shutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	lg.Info("stopped")
	return nil
}
