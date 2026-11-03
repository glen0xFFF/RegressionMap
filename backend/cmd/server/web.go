package main

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// newHandler routes /api and /api/* to api. When webDir is set, every other
// GET/HEAD request is served from webDir, falling back to index.html for paths
// that are not files (SPA client routes). Non-GET/HEAD outside /api is 405.
// Files are opened through os.Root, so neither ".." nor a symlink can reach
// outside webDir.
func newHandler(api http.Handler, webDir string) (http.Handler, error) {
	if webDir == "" {
		return api, nil
	}
	if fi, err := os.Stat(webDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("WEB_DIR %q must be an existing directory", webDir)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Opened per request: holding a handle would lock the directory on
		// Windows and make a rebuild of web/dist fail while the server runs.
		root, err := os.OpenRoot(webDir)
		if err != nil {
			http.Error(w, "WEB_DIR unavailable", http.StatusInternalServerError)
			return
		}
		defer root.Close()
		fsys := root.FS()
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name != "" && name != "index.html" && fs.ValidPath(name) && !strings.Contains(name, `\`) {
			if serveFile(w, r, fsys, name, false) {
				return
			}
		}
		if !serveFile(w, r, fsys, "index.html", true) {
			http.Error(w, "index.html missing from WEB_DIR", http.StatusInternalServerError)
		}
	}), nil
}

// serveFile serves a regular file from fsys; it reports false when name is
// missing, a directory, or not seekable, so the caller can fall back.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string, isIndex bool) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	modTime := fi.ModTime()
	if isIndex {
		// The index references hashed assets; always revalidate it.
		w.Header().Set("Cache-Control", "no-cache")
		modTime = time.Time{}
	}
	http.ServeContent(w, r, name, modTime, rs)
	return true
}
