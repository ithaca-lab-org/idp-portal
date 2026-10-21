package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/session"
)

func mustEnv(name string) string {
	v := os.Getenv(name)
	if v == "" {
		slog.Error("missing required environment variable", "name", name)
		os.Exit(1)
	}
	return v
}

func main() {
	key, err := base64.StdEncoding.DecodeString(mustEnv("SESSION_KEY"))
	if err != nil {
		slog.Error("SESSION_KEY must be base64 (generate with: openssl rand -base64 32)")
		os.Exit(1)
	}
	codec, err := session.NewCodec(key, os.Getenv("COOKIE_INSECURE") != "true")
	if err != nil {
		slog.Error("session key", "err", err)
		os.Exit(1)
	}
	h, err := auth.New(auth.Config{
		Issuer:      mustEnv("OIDC_ISSUER"),
		ClientID:    mustEnv("OIDC_CLIENT_ID"),
		RedirectURL: mustEnv("OIDC_REDIRECT_URL"),
		ExtraCAFile: os.Getenv("OIDC_EXTRA_CA_FILE"),
	}, codec)
	if err != nil {
		slog.Error("auth setup", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.Routes(mux)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	slog.Info("portal-api listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "err", err)
		os.Exit(1)
	}
}
