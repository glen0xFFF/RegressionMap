// Command server runs the RegressionMap API on 127.0.0.1.
//
// Environment:
//
//	WORKSPACE_ROOT             directory whose child folders are the repos to analyse (required, must exist)
//	MODEL_API_KEY              Meta model API key, or
//	MODEL_API_KEY_FILE         path to a file containing only the key
//	MODEL_BASE_URL             default https://api.meta.ai/v1
//	MODEL_ID                   default muse-spark-1.3
//	ADDR                       default 127.0.0.1:8787; must be loopback unless
//	REGRESSIONMAP_ALLOW_REMOTE=1 is set (the API has no authentication)
//	DATA_DIR                   optional directory for persisted state
//	WEB_DIR                    optional path to the built UI (web/dist); served at /
//	LOG_FORMAT                 text (default) or json
//
// SIGINT/SIGTERM trigger a graceful shutdown (10s).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"regressionmap/internal/api"
	"regressionmap/internal/httpx"
	"regressionmap/internal/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "regressionmap:", err)
		os.Exit(1)
	}
}

// buildHandler assembles the API, the optional static UI and the security
// middleware chain (outermost first: access log, panic recovery, security
// headers, Host/Origin checks).
func buildHandler(cfg config, lg *slog.Logger) (http.Handler, *api.Server, error) {
	mc := &model.Client{BaseURL: cfg.ModelBaseURL, APIKey: cfg.Key, Model: cfg.ModelID}
	apiSrv := api.New(api.Config{Root: cfg.Root, Model: mc, DataDir: cfg.DataDir})
	h, err := newHandler(apiSrv, cfg.WebDir)
	if err != nil {
		return nil, nil, err
	}
	return httpx.Chain(h, httpx.AccessLog(lg), httpx.Recover(lg), httpx.SecurityHeaders, httpx.LocalOnlyFor(nil)), apiSrv, nil
}

func run() error {
	cfg, warns, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	lg, err := newLogger(os.Stderr, cfg.LogFormat)
	if err != nil {
		return err
	}

	h, apiSrv, err := buildHandler(cfg, lg)
	if err != nil {
		return err
	}
	if err := apiSrv.Cleanup(context.Background()); err != nil {
		lg.Warn("startup cleanup failed", "error", err.Error())
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	logStartup(lg, cfg, warns)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, newServer(cfg.Addr, h), ln, lg)
}
