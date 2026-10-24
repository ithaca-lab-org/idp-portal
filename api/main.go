package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/ithaca-lab-org/idp-portal/api/internal/api"
	"github.com/ithaca-lab-org/idp-portal/api/internal/auth"
	"github.com/ithaca-lab-org/idp-portal/api/internal/orders"
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

func intEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		slog.Error("environment variable must be a positive integer", "name", name)
		os.Exit(1)
	}
	return n
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	mongoClient, err := mongo.Connect(options.Client().ApplyURI(mustEnv("MONGO_URI")).SetServerSelectionTimeout(10 * time.Second))
	if err != nil {
		slog.Error("mongo connect", "err", err)
		os.Exit(1)
	}
	defer func() { _ = mongoClient.Disconnect(context.Background()) }()
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := mongoClient.Ping(pctx, nil); err != nil {
		cancel()
		slog.Error("mongo unreachable", "err", err)
		os.Exit(1)
	}
	cancel()
	dbName := os.Getenv("MONGO_DB")
	if dbName == "" {
		dbName = "idp"
	}
	store := orders.NewMongoStore(mongoClient.Database(dbName))
	if err := store.EnsureIndexes(ctx); err != nil {
		slog.Error("mongo indexes", "err", err)
		os.Exit(1)
	}
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("kubernetes in-cluster config", "err", err)
		os.Exit(1)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		slog.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	svc := orders.NewService(store, orders.DynamicCluster{Client: dyn}, intEnv("ORDER_CAP_PER_BU", 10), intEnv("ORDER_CAP_PER_USER", 5))
	go svc.RunSyncer(ctx, 5*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.Routes(mux)
	(&api.API{Auth: h, Svc: svc}).Routes(mux)

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
