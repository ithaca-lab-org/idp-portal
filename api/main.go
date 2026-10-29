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
	"github.com/ithaca-lab-org/idp-portal/api/internal/pubsubpub"
	"github.com/ithaca-lab-org/idp-portal/api/internal/secrets"
	"github.com/ithaca-lab-org/idp-portal/api/internal/session"
	"github.com/ithaca-lab-org/idp-portal/api/internal/webui"
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

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// vaultConfigFromEnv reads the Secrets page settings; unset variables keep the defaults.
func vaultConfigFromEnv() secrets.VaultConfig {
	d := secrets.DefaultVaultConfig()
	return secrets.VaultConfig{
		Addr:      envOr("VAULT_ADDR", d.Addr),
		Role:      envOr("VAULT_SECRETS_ROLE", d.Role),
		AuthMount: envOr("VAULT_AUTH_MOUNT", d.AuthMount),
		KVMount:   envOr("VAULT_KV_MOUNT", d.KVMount),
		TokenPath: envOr("VAULT_TOKEN_PATH", d.TokenPath),
	}
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
	restCfg.Timeout = 10 * time.Second // a hung API call must not stall the poller
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		slog.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	svc := orders.NewService(store, orders.DynamicCluster{Client: dyn}, intEnv("ORDER_CAP_PER_BU", 10), intEnv("ORDER_CAP_PER_USER", 5))
	var events *pubsubpub.Publisher
	// Order events are optional: without PUBSUB_TOPIC nothing is published.
	if topic := os.Getenv("PUBSUB_TOPIC"); topic != "" {
		pub, err := pubsubpub.New(ctx, mustEnv("PUBSUB_PROJECT"), topic)
		if err != nil {
			slog.Error("pubsub", "err", err)
			os.Exit(1)
		}
		defer func() { _ = pub.Close() }()
		svc.Events = pub
		events = pub
		slog.Info("publishing order events", "topic", topic)
	}

	// The Secrets page is off unless SECRETS_ENABLED is exactly "true"; off, its routes answer 404.
	var secretSvc *secrets.Service
	if os.Getenv("SECRETS_ENABLED") == "true" {
		vc, err := secrets.NewVaultClient(vaultConfigFromEnv())
		if err != nil {
			slog.Error("vault config", "err", err)
			os.Exit(1)
		}
		sstore := secrets.NewMongoStore(mongoClient.Database(dbName))
		if err := sstore.EnsureIndexes(ctx); err != nil {
			slog.Error("mongo secrets indexes", "err", err)
			os.Exit(1)
		}
		secretSvc = secrets.NewService(vc, secrets.DynamicCluster{Client: dyn}, sstore, store)
		if events != nil {
			secretSvc.Events = events
		}
		slog.Info("secrets page enabled")
	}
	go svc.RunSyncer(ctx, 5*time.Second, 12) // COMPLETE orders are checked every minute

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h.Routes(mux)
	apiHandler := api.New(h, svc)
	apiHandler.Secrets = secretSvc
	apiHandler.Routes(mux)
	mux.Handle("GET /", webui.Handler())

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
