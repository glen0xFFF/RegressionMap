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
//	REGRESSIONMAP_ALLOW_REMOTE=1 is set, which also requires DATA_DIR with at
//	                           least one active admin key
//	DATA_DIR                   optional directory for persisted state and keys.json
//	WEB_DIR                    optional path to the built UI (web/dist); served at /
//	LOG_FORMAT                 text (default) or json
//	ALLOWED_HOSTS              comma-separated extra Host/Origin names (e.g. regressionmap.example.com)
//	RATE_LIMIT_PER_HOUR        per-key POST /api/analyses limit (default 20)
//	WEBHOOK_SECRET             enables completion webhooks (notify_url); HMAC key,
//	                           at least 32 characters; never logged
//	TEST_ENV_ALLOW             comma-separated variable names that test and
//	                           verification runs receive although their names end
//	                           in _KEY, _SECRET, _TOKEN or _PASSWORD (never
//	                           MODEL_API_KEY, MODEL_API_KEY_FILE or WEBHOOK_SECRET);
//	                           git never receives them
//
// Requests that are not local (loopback peer, loopback Host, no
// X-Forwarded-For/Forwarded) need "Authorization: Bearer <key>" on /api/*,
// except GET /api/health. The UI files are served without a key.
//
// Subcommands manage the keys in DATA_DIR (the server may be running):
//
//	server keys create --name NAME --scope admin|analyze
//	server keys list
//	server keys revoke ID
//
// SIGINT/SIGTERM start a graceful shutdown: the server waits up to 10 s for
// open requests, then up to 25 s for running analyses (ended as error) and
// webhooks still being sent.
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
	"regressionmap/internal/apikeys"
	"regressionmap/internal/httpx"
	"regressionmap/internal/model"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		os.Exit(runKeys(os.Args[2:], os.Getenv, os.Stdout, os.Stderr))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "regressionmap:", err)
		os.Exit(1)
	}
}

// buildHandler assembles the API, the optional static UI and the middleware
// chain (outermost first): access log, panic recovery, security headers,
// Host/Origin checks (loopback names plus ALLOWED_HOSTS), API-key auth.
// Rejections by the inner layers are therefore logged and carry the
// security headers.
func buildHandler(cfg config, lg *slog.Logger) (http.Handler, *api.Server, error) {
	mc := &model.Client{BaseURL: cfg.ModelBaseURL, APIKey: cfg.Key, Model: cfg.ModelID}
	apiSrv := api.New(api.Config{Root: cfg.Root, Model: mc, DataDir: cfg.DataDir, WebhookSecret: cfg.WebhookSecret, TestEnvAllow: cfg.TestEnvAllow})
	h, err := newHandler(apiSrv, cfg.WebDir)
	if err != nil {
		return nil, nil, err
	}
	var keys *apikeys.Store
	if cfg.DataDir != "" {
		keys = apikeys.NewStore(cfg.DataDir)
	}
	return httpx.Chain(h,
		httpx.AccessLog(lg),
		httpx.Recover(lg),
		httpx.SecurityHeaders,
		httpx.LocalOnlyFor(httpx.WithDefaultHosts(cfg.AllowedHosts)),
		apikeys.Middleware(apikeys.Options{Store: keys, RateLimitPerHour: cfg.RateLimitPerHour}),
	), apiSrv, nil
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
	// internal/api and the log package write through lg, so with
	// LOG_FORMAT=json every line is JSON.
	slog.SetDefault(lg)

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
	return serveAndDrain(ctx, newServer(cfg.Addr, h), ln, lg, apiSrv)
}
