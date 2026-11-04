// Package notify delivers analysis-completed webhooks (docs/API_CONTRACT.md,
// "Completion notifications (v3)").
//
// A notify URL is treated as a secret: errors produced here never contain its
// path or query, nor an IP address (neither a resolved address nor the
// server's own). Delivery resolves the host itself, refuses the whole
// delivery if any resolved address is not public, and then dials one of the
// vetted addresses, so a DNS rebind between the check and the connection
// cannot redirect it. TLS still verifies the certificate against the
// original host name. Redirects are never followed.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	FormatJSON  = "json"
	FormatSlack = "slack"

	EventCompleted = "analysis.completed"
	UserAgent      = "RegressionMap-Webhook/1"

	MaxURLLength   = 2048
	DefaultTimeout = 5 * time.Second // per attempt
	MaxAttempts    = 3

	disclaimer = "AI-generated hypothesis, not verified."
)

// backoff[i] is the wait before attempt i+2.
var backoff = [MaxAttempts - 1]time.Duration{1 * time.Second, 4 * time.Second}

// ErrBlockedAddress means the host resolved to at least one non-public
// address, or to none at all. Its text never names an address, so the
// result of a delivery cannot be used to map internal DNS names.
var ErrBlockedAddress = errors.New("host does not resolve to a public address")

// Resolver looks up a host's addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ValidFormat reports whether f is a supported notify_format.
func ValidFormat(f string) bool { return f == FormatJSON || f == FormatSlack }

// Validate checks a notify URL against the contract: https only, at most
// MaxURLLength characters, a host, no user info, port 443 or none. Its
// errors never echo the URL.
func Validate(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("notify_url is empty")
	}
	if utf8.RuneCountInString(raw) > MaxURLLength {
		return nil, fmt.Errorf("notify_url is longer than %d characters", MaxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("notify_url is not a valid URL")
	}
	if u.Scheme != "https" {
		return nil, errors.New("notify_url must use https://")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return nil, errors.New("notify_url must have a host")
	}
	if u.User != nil {
		return nil, errors.New("notify_url must not contain user info")
	}
	if p := u.Port(); p != "" && p != "443" {
		return nil, errors.New("notify_url must use port 443 or no port")
	}
	return u, nil
}

// blockedPrefixes are non-public ranges not covered by the netip predicates.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, incl. broadcast
	netip.MustParsePrefix("::/96"),          // deprecated IPv4-compatible
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: may embed a private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local

	// IPv6 that embeds an IPv4 address (v3 addendum).
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated (SIIT)

	// Documentation ranges (v3 addendum).
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("2001:db8::/32"),   // IPv6 documentation
}

// IsPublic reports whether ip is a public unicast address. An IPv4-mapped
// address (::ffff:a.b.c.d) is judged by the IPv4 address it carries.
func IsPublic(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// Hypothesis is the top hypothesis in a json payload.
type Hypothesis struct {
	Rank            int    `json:"rank"`
	Title           string `json:"title"`
	File            string `json:"file"`
	LineStart       int    `json:"line_start"`
	LineEnd         int    `json:"line_end"`
	ConfidenceLabel string `json:"confidence_label"`
}

// Payload is the json body. Field order is the wire order of the contract.
// BaselineRan/CurrentRan only steer the Slack text ("not run").
type Payload struct {
	Event          string      `json:"event"`
	ID             string      `json:"id"`
	Repo           string      `json:"repo"`
	Status         string      `json:"status"`
	Error          string      `json:"error"`
	Baseline       string      `json:"baseline"`
	Current        string      `json:"current"`
	BaselinePassed bool        `json:"baseline_passed"`
	CurrentPassed  bool        `json:"current_passed"`
	ModelStatus    string      `json:"model_status"`
	TopHypothesis  *Hypothesis `json:"top_hypothesis"`
	CreatedAt      string      `json:"created_at"`
	FinishedAt     string      `json:"finished_at"`

	BaselineRan bool `json:"-"`
	CurrentRan  bool `json:"-"`
}

// JSONBody returns the exact bytes of a json-format delivery.
func JSONBody(p Payload) ([]byte, error) {
	p.Event = EventCompleted
	return json.Marshal(p)
}

// slackEscape escapes Slack's control characters and flattens whitespace,
// so untrusted model text cannot ping channels or inject links.
func slackEscape(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// SlackText is the one-paragraph Slack summary. It carries no evidence
// excerpts and no source code, and always ends with the disclaimer.
func SlackText(p Payload) string {
	pf := func(ran, passed bool) string {
		switch {
		case !ran:
			return "not run"
		case passed:
			return "PASS"
		}
		return "FAIL"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "RegressionMap analysis of %s finished with status %s. Baseline %s, HEAD %s.",
		slackEscape(p.Repo, 100), slackEscape(p.Status, 20), pf(p.BaselineRan, p.BaselinePassed), pf(p.CurrentRan, p.CurrentPassed))
	if h := p.TopHypothesis; h != nil {
		loc := slackEscape(h.File, 300)
		if h.LineStart > 0 {
			loc = fmt.Sprintf("%s:%d", loc, h.LineStart)
		}
		fmt.Fprintf(&b, " Top hypothesis: %s (%s, confidence %s).", slackEscape(h.Title, 200), loc, slackEscape(h.ConfidenceLabel, 10))
	} else {
		b.WriteString(" Top hypothesis: none.")
	}
	b.WriteString(" " + disclaimer)
	return b.String()
}

// Result is the outcome of one delivery. Err never contains the URL path or
// query, an IP address or a control character.
type Result struct {
	Delivered  bool
	Attempts   int
	StatusCode int // of the last response, 0 if none
	Err        string
}

// Sender delivers webhooks. The zero value (plus Secret) is the production
// configuration; the other fields are test seams.
type Sender struct {
	Secret []byte // HMAC key for the json format

	Resolver    Resolver                                                          // nil = net.DefaultResolver
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error) // dials a vetted ip:port; nil = net.Dialer
	TLSConfig   *tls.Config                                                       // nil = system roots
	Timeout     time.Duration                                                     // per attempt; 0 = DefaultTimeout
	Sleep       func(ctx context.Context, d time.Duration) error                  // nil = timer bound to ctx
}

func (s *Sender) resolver() Resolver {
	if s.Resolver != nil {
		return s.Resolver
	}
	return net.DefaultResolver
}

func (s *Sender) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// publicOnlyControl re-checks the address at connect time in the default
// dialer, as a second line of defence behind the resolver check.
func publicOnlyControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !IsPublic(ap.Addr()) {
		return ErrBlockedAddress
	}
	return nil
}

// vettedDial resolves the host of addr, rejects the dial if any address is
// non-public, and dials the vetted addresses in order.
func (s *Sender) vettedDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := s.vet(ctx, host)
	if err != nil {
		return nil, err
	}
	return s.dialVetted(ctx, network, ips, port)
}

// vet resolves host and returns its addresses only if every one is public.
// A refusal is the bare ErrBlockedAddress: it never names an address, and a
// name that does not exist or has no address gives the same answer as one
// that resolves to a private address.
func (s *Sender) vet(ctx context.Context, host string) ([]netip.Addr, error) {
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = s.resolver().LookupNetIP(ctx, "ip", host)
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return nil, ErrBlockedAddress
		}
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", host, scrubDNS(err))
		}
	}
	if len(ips) == 0 {
		return nil, ErrBlockedAddress
	}
	for _, ip := range ips {
		if !IsPublic(ip) {
			return nil, ErrBlockedAddress
		}
	}
	return ips, nil
}

// dialVetted dials the vetted ips in order. Without the DialContext seam it
// uses a net.Dialer whose Control re-checks every address at connect time.
// Neither its errors nor those of the connection it returns name an
// address (v3 addendum: last_error never contains a resolved address).
func (s *Sender) dialVetted(ctx context.Context, network string, ips []netip.Addr, port string) (net.Conn, error) {
	dial := s.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: DefaultTimeout, Control: publicOnlyControl}
		dial = d.DialContext
	}
	var errs []error
	for _, ip := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return addrFreeConn{conn}, nil
		}
		errs = append(errs, addrFree(err))
	}
	return nil, errors.Join(errs...)
}

// addrFree drops the addresses a *net.OpError names: the vetted address it
// dialled or talks to, and the server's own address. The operation, the
// network and the cause stay, so errors.Is and Timeout still work.
func addrFree(err error) error {
	oe, ok := err.(*net.OpError)
	if !ok {
		return err
	}
	return &net.OpError{Op: oe.Op, Net: oe.Net, Err: oe.Err}
}

// addrFreeConn is a connection whose errors never name an address. A reset
// or a timeout during the TLS handshake or the request is reported by the
// operation that failed.
type addrFreeConn struct{ net.Conn }

func (c addrFreeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	return n, addrFree(err)
}

func (c addrFreeConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	return n, addrFree(err)
}

func (c addrFreeConn) Close() error { return addrFree(c.Conn.Close()) }

func (c addrFreeConn) SetDeadline(t time.Time) error { return addrFree(c.Conn.SetDeadline(t)) }

func (c addrFreeConn) SetReadDeadline(t time.Time) error { return addrFree(c.Conn.SetReadDeadline(t)) }

func (c addrFreeConn) SetWriteDeadline(t time.Time) error {
	return addrFree(c.Conn.SetWriteDeadline(t))
}

// scrubDNS keeps a DNS error's message without its server address.
func scrubDNS(err error) error {
	var de *net.DNSError
	if errors.As(err, &de) {
		return errors.New(de.Err)
	}
	return err
}

func (s *Sender) client() *http.Client {
	var tc *tls.Config
	if s.TLSConfig != nil {
		tc = s.TLSConfig.Clone()
	} else {
		tc = &tls.Config{}
	}
	tc.ServerName = "" // the transport sets it to the URL's host
	tc.MinVersion = tls.VersionTLS12
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  nil, // a proxy would bypass the vetted dial
			DialContext:            s.vettedDial,
			TLSClientConfig:        tc,
			TLSHandshakeTimeout:    DefaultTimeout,
			ResponseHeaderTimeout:  DefaultTimeout,
			DisableKeepAlives:      true,
			MaxResponseHeaderBytes: 64 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Send delivers p to raw in the given format, retrying network errors, 429
// and 5xx up to MaxAttempts times with 1 s and then 4 s backoff.
func (s *Sender) Send(ctx context.Context, raw, format string, p Payload) Result {
	u, err := Validate(raw)
	if err != nil {
		return Result{Err: err.Error()}
	}
	var body []byte
	switch format {
	case FormatJSON:
		body, err = JSONBody(p)
	case FormatSlack:
		body, err = json.Marshal(map[string]string{"text": SlackText(p)})
	default:
		return Result{Err: "unknown notify format"}
	}
	if err != nil {
		return Result{Err: "cannot encode payload"}
	}
	var sig string
	if format == FormatJSON {
		mac := hmac.New(sha256.New, s.Secret)
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	c := s.client()
	var res Result
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := s.sleep(ctx, backoff[attempt-2]); err != nil {
				res.Err = clean(err, u)
				return res
			}
		}
		res.Attempts = attempt
		code, err := s.attempt(ctx, c, u, body, sig, timeout)
		res.StatusCode = code
		switch {
		case errors.Is(err, ErrBlockedAddress):
			// The fixed text only: a dial error from the connect-time
			// check would otherwise name the address.
			res.Err = ErrBlockedAddress.Error()
			return res
		case err != nil:
			res.Err = clean(err, u)
		case code >= 200 && code < 300:
			res.Delivered, res.Err = true, ""
			return res
		case code == http.StatusTooManyRequests || code >= 500:
			res.Err = fmt.Sprintf("receiver returned HTTP %d", code)
		default:
			res.Err = fmt.Sprintf("receiver returned HTTP %d", code)
			return res
		}
		if ctx.Err() != nil {
			return res
		}
	}
	return res
}

func (s *Sender) attempt(ctx context.Context, c *http.Client, u *url.URL, body []byte, sig string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	if sig != "" {
		req.Header.Set("X-RegressionMap-Event", EventCompleted)
		req.Header.Set("X-RegressionMap-Signature", sig)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// clean renders err without the URL and without addresses: *url.Error is
// unwrapped (its message embeds the full URL), any remaining URL, path or
// query text is removed before and after the control characters (the text
// can come from the receiver, for example a certificate name), and any IP
// address left in it is removed.
func clean(err error, u *url.URL) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	parts := []string{u.String(), u.RawQuery, u.Query().Encode(), u.EscapedPath(), u.Path, u.Fragment}
	// Before: a decoded path can hold a control character. After: one
	// inserted into a part would otherwise hide it.
	msg := removeParts(StripControl(removeParts(err.Error(), parts)), parts)
	msg = dropAddresses(msg)
	if msg == "" {
		msg = "delivery failed"
	}
	return msg
}

func removeParts(msg string, parts []string) string {
	for _, p := range parts {
		if len(p) > 1 {
			msg = strings.ReplaceAll(msg, p, "")
		}
	}
	return msg
}

// addrText matches a run of the characters an IP address, or an address
// with its port, is written with.
var addrText = regexp.MustCompile(`[0-9A-Fa-f.:\[\]]+`)

// dropAddresses removes every IP address, and every address with its port,
// from msg. It is the last guard behind addrFree, for an address that
// reached the error text some other way.
func dropAddresses(msg string) string {
	return addrText.ReplaceAllStringFunc(msg, func(run string) string {
		core := strings.TrimRight(run, ".:")
		if _, err := netip.ParseAddrPort(core); err == nil {
			return run[len(core):]
		}
		if _, err := netip.ParseAddr(core); err == nil {
			return run[len(core):]
		}
		return run
	})
}

// StripControl removes the C0 and C1 control characters (U+0000 to U+001F
// and U+007F to U+009F) from s, so text that came from outside cannot break
// a log line or forge a record.
func StripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
