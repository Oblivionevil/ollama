// Package ollama provides a mobile-binding entry point for the Android app.
// It starts the Go HTTP server that hosts the Ollama desktop UI and Copilot proxy.
//
// Usage from Kotlin/Java:
//
//	val result = Ollama.start(context.filesDir.absolutePath)
//	// result.port  -> HTTP server port
//	// result.token -> auth token for WebView
//	Ollama.stop()
package ollama

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
	"github.com/ollama/ollama/app/ui"
	"github.com/ollama/ollama/app/version"
)

// Result holds the server start result, returned to the Android side.
type Result struct {
	Port  int
	Token string
}

var (
	mu     sync.Mutex
	srv    *http.Server
	cancel context.CancelFunc
)

// Start initialises the Go HTTP server on a random port.
// dataDir should be context.filesDir on Android.
// Returns Result with port and auth token.
func Start(dataDir string) (*Result, error) {
	mu.Lock()
	defer mu.Unlock()

	if srv != nil {
		return nil, fmt.Errorf("server already running")
	}

	// Logging
	logDir := filepath.Join(dataDir, "logs")
	os.MkdirAll(logDir, 0o755)
	logFile, err := os.OpenFile(filepath.Join(logDir, "ollama.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	slog.Info("starting ollama android", "version", version.Version, "dataDir", dataDir)

	// Listen on random port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port
	token := uuid.New().String()

	// Store with explicit DB path inside the app's private storage
	st := &store.Store{
		DBPath: filepath.Join(dataDir, "db.sqlite"),
	}

	// Tools
	registry := tools.NewRegistry()

	// Context
	ctx, cancelFn := context.WithCancel(context.Background())
	cancel = cancelFn

	// UI server
	logger := slog.Default()
	uiServer := &ui.Server{
		Logger:       logger,
		Token:        token,
		Store:        st,
		ToolRegistry: registry,
		WebSearch:    true,
	}

	// HTTP server
	srv = &http.Server{Handler: uiServer.Handler()}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "error", err)
		}
	}()

	// Kick off first-run check (initialises SQLite)
	go func() {
		if _, err := st.HasCompletedFirstRun(); err != nil {
			slog.Warn("first run check failed", "error", err)
		}
		uiServer.UserData(ctx)
	}()

	slog.Info("ollama android server started", "port", port)
	return &Result{Port: port, Token: token}, nil
}

// Stop gracefully shuts down the HTTP server.
func Stop() {
	mu.Lock()
	defer mu.Unlock()

	if srv == nil {
		return
	}

	slog.Info("stopping ollama android server")

	if cancel != nil {
		cancel()
		cancel = nil
	}

	srv.Close()
	srv = nil
}
