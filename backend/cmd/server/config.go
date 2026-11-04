package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"regressionmap/internal/apikeys"
	"regressionmap/internal/model"
	"regressionmap/internal/proc"
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

	AllowedHosts     []string // ALLOWED_HOSTS, extra Host/Origin names (lower-cased)
	RateLimitPerHour int      // RATE_LIMIT_PER_HOUR, per-key POST /api/analyses limit, default 20

	// WebhookSecret is WEBHOOK_SECRET: the HMAC key of completion
	// notifications. "" disables them. It must never be logged.
	WebhookSecret string

	// TestEnvAllow is TEST_ENV_ALLOW: variable names that test and
	// verification runs receive although their names end in _KEY, _SECRET,
	// _TOKEN or _PASSWORD. Git never receives them.
	TestEnvAllow []string
}

// parseAllowedHosts splits ALLOWED_HOSTS on commas, trims and lower-cases
// each entry, skips empty ones, and accepts only bare host names or IP
// addresses (no scheme, port, path or wildcard).
func parseAllowedHosts(v string) ([]string, error) {
	var out []string
	for _, h := range strings.Split(v, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if !validHostname(h) {
			return nil, fmt.Errorf("ALLOWED_HOSTS entry %q is not a host name (use names like regressionmap.example.com, without scheme or port)", h)
		}
		out = append(out, h)
	}
	return out, nil
}

// validHostname accepts an IP address or a DNS name of letters, digits and
// hyphens in labels of 1-63 characters, at most 253 characters in all.
func validHostname(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return !strings.Contains(h, "%")
	}
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func parseRateLimit(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return apikeys.DefaultRateLimitPerHour, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("RATE_LIMIT_PER_HOUR %q must be a positive integer", v)
	}
	return n, nil
}

// checkRemoteKeys enforces that REGRESSIONMAP_ALLOW_REMOTE=1 only starts
// with DATA_DIR set and at least one active admin key in it, so a remote
// deployment is never reachable without someone able to manage keys.
func checkRemoteKeys(dataDir string) error {
	const how = "create one first with: server keys create --name admin --scope admin (with DATA_DIR set)"
	if dataDir == "" {
		return errors.New("REGRESSIONMAP_ALLOW_REMOTE=1 requires DATA_DIR (it holds keys.json with the API keys); " + how)
	}
	ok, err := apikeys.NewStore(dataDir).HasActiveAdmin()
	if err != nil {
		return fmt.Errorf("REGRESSIONMAP_ALLOW_REMOTE=1: cannot read the API keys in DATA_DIR: %v", err)
	}
	if !ok {
		return fmt.Errorf("REGRESSIONMAP_ALLOW_REMOTE=1 requires at least one active admin key in DATA_DIR %q; %s", dataDir, how)
	}
	return nil
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

const minWebhookSecret = 32

// loadWebhookSecret reads WEBHOOK_SECRET exactly as set (receivers verify
// the HMAC with the same bytes). The minimum counts Unicode characters, not
// bytes, as the contract says. Errors never contain the value.
func loadWebhookSecret(getenv func(string) string) (string, error) {
	v := getenv("WEBHOOK_SECRET")
	if v != "" && utf8.RuneCountInString(v) < minWebhookSecret {
		return "", fmt.Errorf("WEBHOOK_SECRET must be at least %d characters (it is the HMAC key of webhook signatures); unset it to disable webhooks", minWebhookSecret)
	}
	return v, nil
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseTestEnvAllow splits TEST_ENV_ALLOW on commas, trims each entry and
// skips empty ones. Every entry must be a variable name, and none may name
// one of the server's own secrets. Errors give an entry's position, never
// its text: a mistaken NAME=value entry would otherwise print the value.
func parseTestEnvAllow(v string) ([]string, error) {
	var out []string
	for i, n := range strings.Split(v, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !envNameRe.MatchString(n) {
			return nil, fmt.Errorf("TEST_ENV_ALLOW entry %d is not a variable name: use letters, digits and underscores, not starting with a digit, and never a value", i+1)
		}
		if proc.AlwaysWithheld(n) {
			return nil, fmt.Errorf("TEST_ENV_ALLOW must not name %s: the server's own secrets are never passed to test runs", strings.ToUpper(n))
		}
		out = append(out, n)
	}
	return out, nil
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
		warns = append(warns, fmt.Sprintf("WARNING: REGRESSIONMAP_ALLOW_REMOTE=1 - listening on non-loopback %s. Non-local /api requests need an API key (Authorization: Bearer); the UI files and GET /api/health are public. Anyone holding a key can run allowlisted test commands.", cfg.Addr))
	}
	if cfg.AllowRemote {
		if err := checkRemoteKeys(cfg.DataDir); err != nil {
			return config{}, nil, err
		}
	}
	if cfg.AllowedHosts, err = parseAllowedHosts(getenv("ALLOWED_HOSTS")); err != nil {
		return config{}, nil, err
	}
	if cfg.RateLimitPerHour, err = parseRateLimit(getenv("RATE_LIMIT_PER_HOUR")); err != nil {
		return config{}, nil, err
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
	if cfg.WebhookSecret, err = loadWebhookSecret(getenv); err != nil {
		return config{}, nil, err
	}
	if cfg.TestEnvAllow, err = parseTestEnvAllow(getenv("TEST_ENV_ALLOW")); err != nil {
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

// newLogger returns the server's logger. The json handler writes C1
// controls, format characters and U+2028/U+2029 inside a string raw (or, for
// U+2028/U+2029, as an escape that decodes back to them), and a terminal or
// log viewer can act on them, so its strings go through escapeLogAttr first.
// The text handler quotes such strings already.
func newLogger(w io.Writer, format string) (*slog.Logger, error) {
	switch format {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: escapeLogAttr})), nil
	}
	return nil, fmt.Errorf("unknown log format %q", format)
}

// escapeLogAttr applies escapeLogText to every string value the json
// handler writes: attribute values of kind string, error values after
// Error(), and the message (the built-in "msg" attribute). Other values are
// left alone.
func escapeLogAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); strings.ContainsFunc(s, logEscaped) {
			a.Value = slog.StringValue(escapeLogText(s))
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok && err != nil {
			if s := err.Error(); strings.ContainsFunc(s, logEscaped) {
				a.Value = slog.StringValue(escapeLogText(s))
			}
		}
	}
	return a
}

// logEscaped reports whether escapeLogText rewrites r: a rune in
// U+0080..U+009F, a format character (unicode.Cf) or U+2028/U+2029. C0
// controls are left to the json handler, which escapes them already.
func logEscaped(r rune) bool {
	return r >= 0x80 && r <= 0x9f || r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r)
}

// escapeLogText writes each rune for which logEscaped is true as the
// six-character text \uXXXX in lowercase hex; a format character beyond
// U+FFFF becomes its UTF-16 surrogate pair, two such texts. Every other
// byte, including invalid UTF-8, is kept for the handler as it was.
func escapeLogText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case !logEscaped(r):
			b.WriteString(s[i : i+size])
		case r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += size
	}
	return b.String()
}

// logStartup logs where the server listens. It logs whether a key and a
// webhook secret are configured, never their values, and the names (never
// the values) of the variables test runs do not receive.
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
		"allowed_hosts", strings.Join(cfg.AllowedHosts, ","),
		"rate_limit_per_hour", cfg.RateLimitPerHour,
		"webhooks_enabled", cfg.WebhookSecret != "",
		"test_env_withheld", strings.Join(proc.WithheldNames(cfg.TestEnvAllow), ","),
		"test_env_allow", strings.Join(cfg.TestEnvAllow, ","),
	)
}
