package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mattmezza/sm/internal/config"
	"github.com/mattmezza/sm/internal/mail"
	"github.com/mattmezza/sm/internal/server"
	"github.com/mattmezza/sm/internal/store"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "", "path to config.toml (default $SM_CONFIG or ./config.toml)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("sm", version)
		return
	}
	path := *cfgPath
	if path == "" {
		path = os.Getenv("SM_CONFIG")
	}
	if path == "" {
		path = "config.toml"
	}

	cfg, err := config.Load(path)
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DB.Path), 0o750); err != nil { // #nosec G703 -- path comes from the admin's own config file
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(cfg.DB.Path)
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	// Root context cancelled on SIGINT/SIGTERM drives clean shutdown of the
	// sync workers and the HTTP server.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := mail.NewManager(cfg, st)
	mgr.Start(ctx)

	srv, err := server.New(cfg, st, mgr, version)
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	slog.Info("sm listening", "addr", addr, "version", version)
	// No global write timeout: /events is a long-lived SSE stream.
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("shutdown", "err", err)
	}
}
