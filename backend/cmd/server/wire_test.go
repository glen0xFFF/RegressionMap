package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"regressionmap/internal/api"
	"regressionmap/internal/fixture"
	"regressionmap/internal/model"
	"regressionmap/internal/notify"
	"regressionmap/internal/store"
)

func TestBuildHandlerAppliesSecurityChain(t *testing.T) {
	cfg := config{Root: t.TempDir(), DataDir: t.TempDir(), ModelID: "muse-spark-1.3"}
	h, _, err := buildHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health: %d %v", rec.Code, rec.Header())
	}

	req = httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "evil.example:8787"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden_host") {
		t.Fatalf("rebinding host: %d %s", rec.Code, rec.Body)
	}

	req = httptest.NewRequest("POST", "/api/analyses", strings.NewReader("repo=x"))
	req.Host = "127.0.0.1:8787"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form csrf: %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/analyses", nil)
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:50000" // v2: only a loopback peer is local (httptest defaults to 192.0.2.1)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list endpoint: %d %s", rec.Code, rec.Body)
	}
}

func localPOST(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Host = "127.0.0.1:8787"
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// WEBHOOK_SECRET reaches api.Config: with it notify_url is validated, without
// it any notify_url is refused. The access log never holds the URL.
func TestBuildHandlerWiresWebhookSecret(t *testing.T) {
	const hook = "https://hooks.example.com/services/T0/B0/PATHSECRET?token=QUERYSECRET"
	body := func(u string) string {
		return `{"repo":"nope","baseline":"HEAD","command_id":"node-test","notify_url":"` + u + `"}`
	}
	var logs bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logs, nil))

	on, _, err := buildHandler(config{Root: t.TempDir(), ModelID: "m", WebhookSecret: strings.Repeat("s", 32)}, lg)
	if err != nil {
		t.Fatal(err)
	}
	if rec := localPOST(on, "/api/analyses", body("http://hooks.example.com/x")); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_notify_url") {
		t.Fatalf("with secret, http URL: %d %s", rec.Code, rec.Body)
	}
	// A valid URL passes the notify checks and fails later, on the repo.
	if rec := localPOST(on, "/api/analyses", body(hook)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "invalid_repo") {
		t.Fatalf("with secret, valid URL: %d %s", rec.Code, rec.Body)
	}

	off, _, err := buildHandler(config{Root: t.TempDir(), ModelID: "m"}, lg)
	if err != nil {
		t.Fatal(err)
	}
	if rec := localPOST(off, "/api/analyses", body(hook)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "webhooks_disabled") {
		t.Fatalf("without secret: %d %s", rec.Code, rec.Body)
	}
	for _, leak := range []string{"PATHSECRET", "QUERYSECRET", "/services/"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("access log leaks %q: %s", leak, logs.String())
		}
	}
}

// ---- shutdown (v3 addendum) ----------------------------------------------

func TestShutdownBudgets(t *testing.T) {
	if shutdownTimeout != 10*time.Second || drainTimeout != 25*time.Second {
		t.Fatalf("shutdown %v, drain %v; want 10s then 25s", shutdownTimeout, drainTimeout)
	}
}

// gateNotifier blocks every delivery until open is closed.
type gateNotifier struct {
	mu    sync.Mutex
	calls []notify.Payload
	done  int
	open  chan struct{}
}

func (g *gateNotifier) Send(_ context.Context, _, _ string, p notify.Payload) notify.Result {
	g.mu.Lock()
	g.calls = append(g.calls, p)
	g.mu.Unlock()
	<-g.open
	g.mu.Lock()
	g.done++
	g.mu.Unlock()
	return notify.Result{Delivered: true, Attempts: 1, StatusCode: 200}
}

func (g *gateNotifier) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls), g.done
}

func postAnalysis(t *testing.T, base, repo, baseline, cmd string) string {
	t.Helper()
	body := `{"repo":"` + repo + `","baseline":"` + baseline + `","command_id":"` + cmd + `","notify_url":"https://hooks.example.com/services/T0/B0/PATHSECRET"}`
	resp, err := http.Post(base+"/api/analyses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a api.Analysis
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil || resp.StatusCode != 202 {
		t.Fatalf("POST %s: %d %v", repo, resp.StatusCode, err)
	}
	return a.ID
}

func storedAnalysis(t *testing.T, data, id string) api.Analysis {
	t.Helper()
	b, err := store.New(data).Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var a api.Analysis
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// On SIGTERM the process waits, after the HTTP shutdown, for a delivery in
// flight, and ends a running analysis as error, whose notification is then
// sent too.
func TestServeAndDrainWaitsForNotifications(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	shas := map[string]fixture.Shas{}
	for _, r := range []string{"r1", "r2"} {
		sh, err := fixture.Build(filepath.Join(root, r))
		if err != nil {
			t.Fatal(err)
		}
		shas[r] = sh
	}
	gate := &gateNotifier{open: make(chan struct{})}
	var once sync.Once
	open := func() { once.Do(func() { close(gate.open) }) }
	t.Cleanup(open)
	node := func(id string, ms int) api.Command {
		return api.Command{ID: id, Argv: []string{"node", "-e", fmt.Sprintf("setTimeout(()=>{},%d)", ms)}, Description: id}
	}
	apiSrv := api.New(api.Config{
		Root: root, Model: &model.Client{Model: "m"}, DataDir: data,
		WebhookSecret: strings.Repeat("s", 32), Notifier: gate,
		Commands: []api.Command{node("quick", 50), node("stuck", 20000)},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	var logs bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveAndDrain(ctx, newServer(ln.Addr().String(), apiSrv), ln, lg, apiSrv) }()

	quick := postAnalysis(t, base, "r1", shas["r1"].Baseline, "quick")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if calls, _ := gate.counts(); calls == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first delivery never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stuck := postAnalysis(t, base, "r2", shas["r2"].Baseline, "stuck")
	time.Sleep(300 * time.Millisecond) // the stuck analysis is under way

	cancel() // what SIGTERM does through signal.NotifyContext
	select {
	case err := <-done:
		t.Fatalf("the process returned (%v) while a delivery was in flight", err)
	case <-time.After(500 * time.Millisecond):
	}
	open()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveAndDrain: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the process did not return after the deliveries ended")
	}

	if calls, finished := gate.counts(); calls != 2 || finished != 2 {
		t.Fatalf("deliveries started %d, finished %d; want 2 and 2", calls, finished)
	}
	if a := storedAnalysis(t, data, quick); a.Status != "done" || a.Notification == nil || a.Notification.Status != "delivered" {
		t.Fatalf("quick analysis: %s %+v", a.Status, a.Notification)
	}
	a := storedAnalysis(t, data, stuck)
	if a.Status != "error" || a.Error != "analysis interrupted: the server stopped while it was running" ||
		a.Notification == nil || a.Notification.Status != "delivered" {
		t.Fatalf("stuck analysis: %s %q %+v", a.Status, a.Error, a.Notification)
	}
	if !strings.Contains(logs.String(), "draining") {
		t.Errorf("drain not logged: %s", logs.String())
	}
}

type blockingDrainer struct{ called chan struct{} }

// Drain returns what api.Server.Drain returns when its budget runs out.
func (b blockingDrainer) Drain(ctx context.Context) error {
	close(b.called)
	<-ctx.Done()
	return fmt.Errorf("1 analysis run(s) still stopping, 0 notification(s) still being sent: %w", ctx.Err())
}

// The drain stops waiting when its budget runs out, and logs what it was
// still waiting for, as Drain reports it, instead of a fixed guess.
func TestDrainGivesUpAfterItsBudget(t *testing.T) {
	var logs bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&logs, nil))
	d := blockingDrainer{called: make(chan struct{})}
	start := time.Now()
	drain(d, lg, 100*time.Millisecond)
	select {
	case <-d.called:
	default:
		t.Fatal("Drain was not called")
	}
	if el := time.Since(start); el < 100*time.Millisecond || el > 5*time.Second {
		t.Fatalf("drain returned after %v, want about 100ms", el)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "drain") {
		t.Fatalf("an incomplete drain must be logged as a warning: %s", logs.String())
	}
	const want = `level=WARN msg="drain incomplete" error="1 analysis run(s) still stopping, 0 notification(s) still being sent: context deadline exceeded"`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the warning must carry Drain's error\n got: %s\nwant: %s", logs.String(), want)
	}
}

// run cannot be started by a test (it waits for SIGINT or SIGTERM), so its
// two shutdown and logging wires are checked on main.go's syntax tree: lg
// becomes the slog default, so internal/api and the log package write
// through it, and run ends with serveAndDrain over the api server.
func TestRunWiresSlogDefaultAndDrain(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "run" {
			run = fd
		}
	}
	if run == nil {
		t.Fatal("main.go has no func run")
	}
	src := func(n ast.Node) string {
		var b bytes.Buffer
		if err := format.Node(&b, fset, n); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	setDefault, drained := -1, -1
	for i, st := range run.Body.List {
		switch st := st.(type) {
		case *ast.ExprStmt:
			if src(st) == "slog.SetDefault(lg)" {
				setDefault = i
			}
		case *ast.ReturnStmt:
			if len(st.Results) == 1 && strings.HasPrefix(src(st.Results[0]), "serveAndDrain(") && strings.HasSuffix(src(st.Results[0]), ", lg, apiSrv)") {
				drained = i
			}
		}
	}
	if setDefault < 0 {
		t.Error("run does not call slog.SetDefault(lg)")
	}
	if drained < 0 {
		t.Error("run does not end with serveAndDrain(..., lg, apiSrv)")
	}
	if setDefault >= 0 && drained >= 0 && setDefault > drained {
		t.Error("slog.SetDefault(lg) comes after serveAndDrain")
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
