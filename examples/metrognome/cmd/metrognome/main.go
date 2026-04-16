package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/birdayz/protobuf-ecosystem/protoconfig"

	metrognomev1 "github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/auth"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/billing"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/consumer"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/meter"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/services"
	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/internal/storage"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

	defaults := &metrognomev1.Config{
		ListenAddress: ":8080",
		FrontendUrl:   "http://localhost:3000",
	}
	configPath := "config.yaml"
	if v := os.Getenv("CONFIG_FILE"); v != "" {
		configPath = v
	}
	cfg, err := protoconfig.Load(configPath, defaults)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("no config.yaml found, using defaults")
			cfg = defaults
		} else {
			slog.Error("failed to load config", "error", err)
			os.Exit(1)
		}
	}

	// Environment overrides for deployment
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddress = v
	}
	if v := os.Getenv("FRONTEND_URL"); v != "" {
		cfg.FrontendUrl = v
	}
	if clientID := os.Getenv("GITHUB_CLIENT_ID"); clientID != "" {
		if cfg.GithubOauth == nil {
			cfg.GithubOauth = &metrognomev1.GitHubOAuth{}
		}
		cfg.GithubOauth.ClientId = clientID
		cfg.GithubOauth.ClientSecret = os.Getenv("GITHUB_CLIENT_SECRET")
		cfg.GithubOauth.RedirectUrl = os.Getenv("GITHUB_REDIRECT_URL")
	}

	// Connect to FoundationDB
	fdb.MustAPIVersion(720)
	var fdbDB fdb.Database
	if cfg.FdbClusterFile != "" {
		fdbDB, err = fdb.OpenDatabase(cfg.FdbClusterFile)
		if err != nil {
			slog.Error("failed to open FDB", "error", err)
			os.Exit(1)
		}
	} else {
		fdbDB = fdb.MustOpenDefault()
	}
	slog.Info("connected to FoundationDB")

	recordDB := rl.NewFDBDatabase(fdbDB)

	// Initialize storage
	db, err := storage.NewDB(recordDB)
	if err != nil {
		slog.Error("failed to initialize storage", "error", err)
		os.Exit(1)
	}

	// Initialize dynamic meter engine
	meterEngine := meter.NewEngine(recordDB, subspace.Sub("metrognome_meters"))

	// Load existing meters from storage and register them in the dynamic engine
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer startupCancel()
	existingMeters, _, err := db.Meters().List(startupCtx, 0, nil)
	if err != nil {
		slog.Warn("failed to load existing meters", "error", err)
	} else {
		for _, m := range existingMeters {
			if err := meterEngine.Register(m); err != nil {
				slog.Warn("failed to register meter", "slug", m.GetSlug(), "error", err)
			} else {
				slog.Info("registered meter", "slug", m.GetSlug())
			}
		}
	}

	// Initialize billing engine
	billingEngine := billing.NewEngine(recordDB, db.MetaData(), db.Subspace())

	// Kafka consumer (nil if not configured)
	var kafkaConsumer *consumer.Consumer

	// Set up HTTP mux with ConnectRPC services
	mux := http.NewServeMux()

	// Health check (liveness)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","service":"metrognome"}`)
	})

	// Readiness probe — tests FDB connectivity
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		_, err := recordDB.Run(ctx, func(rtx *rl.FDBRecordContext) (any, error) {
			store, err := rl.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(db.MetaData()).
				SetSubspace(db.Subspace()).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			count, err := store.GetRecordCount()
			if err != nil {
				return nil, err
			}
			return count, nil
		})
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})

	// Consumer lag endpoint
	mux.HandleFunc("GET /consumer-lag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if kafkaConsumer == nil {
			fmt.Fprint(w, `{"enabled":false}`)
			return
		}
		lags := kafkaConsumer.GetLag()
		resp := map[string]any{"enabled": true, "partitions": lags}
		json.NewEncoder(w).Encode(resp)
	})

	// Auth (GitHub OAuth) — optional, skip if not configured
	var connectOpts []connect.HandlerOption
	if gh := cfg.GithubOauth; gh != nil && gh.GetClientId() != "" {
		authHandler := auth.NewHandler(gh, cfg.FrontendUrl, db)
		authHandler.RegisterRoutes(mux)
		connectOpts = append(connectOpts, connect.WithInterceptors(authHandler.Interceptor()))
		slog.Info("github oauth enabled")
	}

	// Register all services
	register := func(path string, handler http.Handler) {
		mux.Handle(path, handler)
	}

	register(metrognomev1connect.NewCustomerServiceHandler(services.NewCustomerService(db.Customers()), connectOpts...))
	register(metrognomev1connect.NewMeterServiceHandler(services.NewMeterService(db.Meters(), meterEngine), connectOpts...))
	register(metrognomev1connect.NewPlanServiceHandler(services.NewPlanService(db.Plans(), db.Charges()), connectOpts...))
	register(metrognomev1connect.NewContractServiceHandler(services.NewContractService(db.Contracts()), connectOpts...))
	register(metrognomev1connect.NewEventServiceHandler(services.NewEventService(db.Events(), db.Alerts(), meterEngine), connectOpts...))
	register(metrognomev1connect.NewInvoiceServiceHandler(services.NewInvoiceService(db.Invoices(), db.Contracts(), billingEngine), connectOpts...))
	register(metrognomev1connect.NewCreditServiceHandler(services.NewCreditService(db.Credits()), connectOpts...))
	register(metrognomev1connect.NewAlertServiceHandler(services.NewAlertService(db.Alerts()), connectOpts...))
	register(metrognomev1connect.NewProductServiceHandler(services.NewProductService(db.Products()), connectOpts...))
	register(metrognomev1connect.NewRateCardServiceHandler(services.NewRateCardService(db.RateCards(), db.Rates()), connectOpts...))
	register(metrognomev1connect.NewApiKeyServiceHandler(services.NewApiKeyService(db.ApiKeys()), connectOpts...))

	// Start Kafka consumer if configured
	consumerCtx, consumerCancel := context.WithCancel(context.Background())
	defer consumerCancel()
	if k := cfg.Kafka; k != nil && len(k.Brokers) > 0 && k.Topic != "" {
		groupID := k.GroupId
		if groupID == "" {
			groupID = "metrognome"
		}
		batchSize := int(k.BatchSize)
		if batchSize <= 0 {
			batchSize = 100
		}
		kafkaConsumer, err = consumer.New(consumer.Config{
			Brokers:   k.Brokers,
			Topic:     k.Topic,
			GroupID:   groupID,
			BatchSize: batchSize,
		}, db, meterEngine, slog.Default())
		if err != nil {
			slog.Error("failed to create kafka consumer", "error", err)
			os.Exit(1)
		}
		go func() {
			if err := kafkaConsumer.Run(consumerCtx); err != nil && err != context.Canceled {
				slog.Error("kafka consumer error", "error", err)
			}
		}()
		slog.Info("kafka consumer started", "brokers", k.Brokers, "topic", k.Topic)
	}

	// Static files with SPA fallback — serve frontend from STATIC_DIR if set.
	if staticDir := os.Getenv("STATIC_DIR"); staticDir != "" {
		fs := http.FileServer(http.Dir(staticDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			// Try serving the file directly.
			path := staticDir + r.URL.Path
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				// Hashed assets (Vite adds content hash) → cache forever.
				// index.html → no cache so deploys take effect immediately.
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
				}
				fs.ServeHTTP(w, r)
				return
			}
			// SPA fallback: serve index.html for client-side routing.
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			http.ServeFile(w, r, staticDir+"/index.html")
		})
		slog.Info("serving static files", "dir", staticDir)
	}

	// CORS for frontend
	handler := services.CORSMiddleware(cfg.FrontendUrl, mux)

	srv := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("metrognome starting", "addr", cfg.ListenAddress)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := <-done
	slog.Info("shutting down", "signal", sig)
	consumerCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	slog.Info("metrognome stopped")
}
