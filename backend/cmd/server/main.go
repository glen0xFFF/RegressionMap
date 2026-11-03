// Command server runs the RegressionMap API on 127.0.0.1.
//
// Environment:
//
//	WORKSPACE_ROOT      directory whose child folders are the repos to analyse (required)
//	MODEL_API_KEY       Meta model API key, or
//	MODEL_API_KEY_FILE  path to a file containing only the key
//	MODEL_BASE_URL      default https://api.meta.ai/v1
//	MODEL_ID            default muse-spark-1.3
//	ADDR                default 127.0.0.1:8787
package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"regressionmap/internal/api"
	"regressionmap/internal/model"
)

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

func main() {
	root := os.Getenv("WORKSPACE_ROOT")
	if root == "" {
		log.Fatal("WORKSPACE_ROOT is required (a folder containing git repositories)")
	}
	key, err := loadKey(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	mc := &model.Client{
		BaseURL: or(os.Getenv("MODEL_BASE_URL"), model.DefaultBaseURL),
		APIKey:  key,
		Model:   or(os.Getenv("MODEL_ID"), model.DefaultModel),
	}
	addr := or(os.Getenv("ADDR"), "127.0.0.1:8787")
	log.Printf("RegressionMap API on http://%s  workspace=%s  model=%s configured=%v", addr, root, mc.Model, mc.Configured())
	srv := &http.Server{Addr: addr, Handler: api.New(api.Config{Root: root, Model: mc}), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
