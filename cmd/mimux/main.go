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
	"strings"
	"syscall"
	"time"

	"github.com/mattmezza/mimux/internal/config"
	"github.com/mattmezza/mimux/internal/mail"
	"github.com/mattmezza/mimux/internal/server"
	"github.com/mattmezza/mimux/internal/store"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("mimux", version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DB.Path), 0o750); err != nil { // #nosec G703 -- path comes from the admin's own config file
		slog.Error("startup", "err", err)
		os.Exit(1)
	}
	migrateLegacyDB(cfg.DB.Path)
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
	slog.Info("mimux listening", "addr", addr, "version", version)
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

// migrateLegacyDB renames a pre-rename sm.db (plus its WAL/SHM sidecars) to the
// mimux.db the config now points at. Without it an existing install would boot
// onto a fresh empty database sitting next to the populated old one, which
// presents as total data loss. Everything else — bodies, attachments, the FTS
// search index — lives inside that same file, so there is nothing else to move.
// A failed rename is fatal rather than ignored, for the same reason.
// ponytail: filename swap only; the data directory itself was never named
// after the app.
// TODO(v0.21): drop, together with the SM_ env fallback in internal/config.
func migrateLegacyDB(newPath string) {
	dir, base := filepath.Dir(newPath), filepath.Base(newPath)
	if !strings.Contains(base, "mimux") {
		return // custom filename: not ours to guess at
	}
	oldPath := filepath.Join(dir, strings.Replace(base, "mimux", "sm", 1))
	if _, err := os.Stat(newPath); err == nil {
		return // already migrated (or a genuinely new install)
	}
	if _, err := os.Stat(oldPath); err != nil {
		return // nothing to migrate
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(oldPath+suffix, newPath+suffix); err != nil && !os.IsNotExist(err) {
			slog.Error("startup: rename legacy database", "from", oldPath+suffix, "to", newPath+suffix, "err", err)
			os.Exit(1)
		}
	}
	slog.Info("renamed legacy database from the sm -> mimux rename", "from", oldPath, "to", newPath)
}
