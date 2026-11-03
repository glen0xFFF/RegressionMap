package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"regressionmap/internal/model"
)

const defaultAddr = "127.0.0.1:8787"

// config is everything main needs, loaded from the environment.
// Key is the model API key; it must never be logged.
type config struct {
	Root         string // WORKSPACE_ROOT, required, existing directory
	Addr         string // ADDR, default 127.0.0.1:8787, loopback unless AllowRemote
	AllowRemote  bool   // REGRESSIONMAP_ALLOW_REMOTE=1
	DataDir      string // DATA_DIR, optional
	WebDir       string // WEB_DIR, optional built web/dist (must contain index.html)
	LogFormat    string // LOG_FORMAT text|json, default text
	Key          string // MODEL_API_KEY or MODEL_API_KEY_FILE
	ModelBaseURL string // MODEL_BASE_URL
	ModelID      string // MODEL_ID
}

func loadKey(getenv func(string) string) (string, error) {
	if k := strings.TrimSpace(getenv("MODEL_API_KEY")); k != "" {
		return k, nil
	}
	p := getenv("MODEL_API_KEY_FILE")
	if p == "" {
		return "", nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", errors.New("MODEL_API_KEY_FILE is set but the file cannot be read")
	}
	return strings.TrimSpace(string(b)), nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// loadConfig validates the environment. Warnings are returned for main to
// log loudly; an error means the server must not start.
func loadConfig(getenv func(string) string) (config, []string, error) {
	var warns []string
	cfg := config{
		Root:         getenv("WORKSPACE_ROOT"),
		Addr:         or(getenv("ADDR"), defaultAddr),
		AllowRemote:  getenv("REGRESSIONMAP_ALLOW_REMOTE") == "1",
		DataDir:      getenv("DATA_DIR"),
		WebDir:       getenv("WEB_DIR"),
		LogFormat:    or(getenv("LOG_FORMAT"), "text"),
		ModelBaseURL: or(getenv("MODEL_BASE_URL"), model.DefaultBaseURL),
		ModelID:      or(getenv("MODEL_ID"), model.DefaultModel),
	}

	if cfg.Root == "" {
		return config{}, nil, errors.New("WORKSPACE_ROOT is required (a folder containing git repositories)")
	}
	if fi, err := os.Stat(cfg.Root); err != nil || !fi.IsDir() {
		return config{}, nil, fmt.Errorf("WORKSPACE_ROOT %q must be an existing directory", cfg.Root)
	}

	loop, err := isLoopbackAddr(cfg.Addr)
	if err != nil {
		return config{}, nil, fmt.Errorf("ADDR %q: %v", cfg.Addr, err)
	}
	if !loop {
		if !cfg.AllowRemote {
			return config{}, nil, fmt.Errorf("ADDR %q is not a loopback address; refusing to expose the API (it runs git and test commands). Set REGRESSIONMAP_ALLOW_REMOTE=1 to override", cfg.Addr)
		}
		warns = append(warns, fmt.Sprintf("WARNING: REGRESSIONMAP_ALLOW_REMOTE=1 - listening on non-loopback %s. The API has no authentication and can run allowlisted test commands; anyone who can reach this address can use it.", cfg.Addr))
	}

	if cfg.WebDir != "" {
		if fi, err := os.Stat(cfg.WebDir); err != nil || !fi.IsDir() {
			return config{}, nil, fmt.Errorf("WEB_DIR %q must be an existing directory (the built web/dist)", cfg.WebDir)
		}
		if fi, err := os.Stat(filepath.Join(cfg.WebDir, "index.html")); err != nil || fi.IsDir() {
			return config{}, nil, fmt.Errorf("WEB_DIR %q has no index.html; run npm run build in web/", cfg.WebDir)
		}
	}

	switch cfg.LogFormat {
	case "text", "json":
	default:
		return config{}, nil, fmt.Errorf("LOG_FORMAT %q must be text or json", cfg.LogFormat)
	}

	if cfg.Key, err = loadKey(getenv); err != nil {
		return config{}, nil, err
	}
	return cfg, warns, nil
}

// isLoopbackAddr reports whether host:port binds only to loopback.
// An empty host (":8787") binds every interface and is not loopback.
func isLoopbackAddr(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, err
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}

func newLogger(w io.Writer, format string) (*slog.Logger, error) {
	switch format {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, nil)), nil
	}
	return nil, fmt.Errorf("unknown log format %q", format)
}

// logStartup logs where the server listens. It logs whether a key is
// configured, never the key.
func logStartup(lg *slog.Logger, cfg config, warns []string) {
	for _, w := range warns {
		lg.Warn(w)
	}
	lg.Info("RegressionMap API starting",
		"url", "http://"+cfg.Addr,
		"addr", cfg.Addr,
		"workspace", cfg.Root,
		"model_id", cfg.ModelID,
		"model_configured", cfg.Key != "",
		"web_dir", cfg.WebDir,
		"data_dir", cfg.DataDir,
	)
}
