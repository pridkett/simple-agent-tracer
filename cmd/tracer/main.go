package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	app "github.com/pridkett/simple_agent_tracer"
	"github.com/pridkett/simple_agent_tracer/internal/auth"
	"github.com/pridkett/simple_agent_tracer/internal/httpapi"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func run() error {
	bind := flag.String("bind", env("TRACER_BIND", "127.0.0.1:4318"), "HTTP bind address")
	database := flag.String("database", env("TRACER_DATABASE_URL", "tracer.db"), "SQLite path or PostgreSQL URL")
	flag.Parse()
	host, _, err := net.SplitHostPort(*bind)
	if err != nil {
		return errors.New("invalid bind address")
	}
	ip, _ := netip.ParseAddr(host)
	loopback := ip.Unmap().IsLoopback()
	local := loopback && os.Getenv("TRACER_OIDC_ISSUER") == ""
	switch os.Getenv("TRACER_AUTH_MODE") {
	case "", "auto":
	case "local":
		if !loopback {
			return errors.New("local mode requires a loopback IP bind")
		}
		local = true
	case "authenticated":
		local = false
	default:
		return errors.New("invalid TRACER_AUTH_MODE")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	store, err := storage.Open(setup, *database)
	if err != nil {
		return fmt.Errorf("initialize database (%T)", err)
	}
	defer store.Close()
	for slug := range strings.SplitSeq(os.Getenv("TRACER_PROJECTS"), ",") {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		if _, err := store.CreateProject(setup, slug, 30, true); err != nil {
			if _, lookup := store.Project(setup, slug); lookup != nil {
				return fmt.Errorf("unable to provision configured project %q", slug)
			}
		}
	}
	access, credentials, err := auth.New(setup, local, os.Getenv("TRACER_CREDENTIALS"), auth.OIDCConfig{
		Issuer: os.Getenv("TRACER_OIDC_ISSUER"), Client: os.Getenv("TRACER_OIDC_CLIENT_ID"),
		Secret: os.Getenv("TRACER_OIDC_CLIENT_SECRET"), Callback: os.Getenv("TRACER_OIDC_CALLBACK"),
		AdminGroup: os.Getenv("TRACER_OIDC_ADMIN_GROUP"), LegacyOwnerSubject: os.Getenv("TRACER_OIDC_OWNER_SUBJECT"),
	})
	if err != nil {
		return err
	}
	for _, credential := range credentials {
		if _, err := store.Project(setup, credential.Project); err != nil {
			return fmt.Errorf("credential project %q is unavailable", credential.Project)
		}
	}
	listener, err := net.Listen("tcp", *bind)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{
		Handler: httpapi.New(store, access, app.Frontend(), app.OpenAPI), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	sweepDone := make(chan struct{})
	go func() { defer close(sweepDone); sweep(ctx, store) }()
	defer func() { stop(); <-sweepDone }()
	slog.Info("tracer listening", "address", listener.Addr(), "local", local)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func sweep(ctx context.Context, store *storage.Store) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		now := time.Now()
		count, err := store.Sweep(ctx, now)
		if err != nil && ctx.Err() == nil {
			slog.Warn("retention sweep failed", "errorType", fmt.Sprintf("%T", err))
		}
		if count > 0 {
			slog.Info("expired traces removed", "count", count)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func main() {
	if err := run(); err != nil {
		slog.Error("tracer stopped", "reason", err)
		os.Exit(1)
	}
}
