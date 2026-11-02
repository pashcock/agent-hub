// Command agent-hub-preview serves an isolated, read-only UI over a consistent
// snapshot. It never starts adapters, recovers sessions, or exposes MCP/actions.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pashcock/agent-hub/internal/hub"
	"github.com/pashcock/agent-hub/internal/semantic"
	"github.com/pashcock/agent-hub/internal/store"
	"github.com/pashcock/agent-hub/internal/webui"
)

func main() {
	source := flag.String("db", "", "source database; opened read-only and copied")
	addr := flag.String("addr", "127.0.0.1:17777", "loopback preview address")
	python := flag.String("semantic-python", "", "existing model2vec Python executable")
	model := flag.String("semantic-model", "", "existing model2vec model directory")
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("preview address must be loopback")
	}
	if *source == "" {
		log.Fatal("-db is required")
	}
	dir, err := os.MkdirTemp("", "agent-hub-preview-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path, err := filepath.Abs(*source)
	if err != nil {
		log.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	src, err := sql.Open("sqlite", uri.String())
	if err != nil {
		log.Fatal(err)
	}
	snapshot := filepath.Join(dir, "hub.db")
	_, err = src.Exec("VACUUM INTO ?", snapshot)
	src.Close()
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(snapshot)
	if err != nil {
		log.Fatal(err)
	}
	// Migrate only the copy, then enforce read-only at SQLite connection level.
	if err = st.Close(); err != nil {
		log.Fatal(err)
	}
	st, err = store.OpenReadOnly(snapshot)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	h := hub.NewSnapshot(st)
	defer h.Close()
	ui, err := webui.New(h, st)
	if err != nil {
		log.Fatal(err)
	}
	ui.Preview = true
	if *python != "" || *model != "" {
		worker, err := semantic.New(semantic.Config{Python: *python, ModelPath: *model}, filepath.Join(dir, "semantic-vectors.db"))
		if err != nil {
			log.Fatal(err)
		}
		defer worker.Close()
		ui.Semantic = worker
	}
	mux := http.NewServeMux()
	ui.Register(mux)
	srv := &http.Server{Addr: *addr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path == "/mcp" {
			http.Error(w, "Preview is read-only; no agent control", 403)
			return
		}
		mux.ServeHTTP(w, r)
	})}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	fmt.Printf("Read-only snapshot preview: http://%s/\nSnapshot: %s\n", *addr, snapshot)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
