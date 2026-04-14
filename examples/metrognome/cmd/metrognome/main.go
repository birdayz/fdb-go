package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/birdayz/fdb-record-layer-go/examples/metrognome/gen/metrognome/v1/metrognomev1connect"
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

	listenAddr := ":8080"
	if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
		listenAddr = addr
	}

	clusterFile := os.Getenv("FDB_CLUSTER_FILE")

	// Connect to FoundationDB
	fdb.MustAPIVersion(720)
	var fdbDB fdb.Database
	var err error
	if clusterFile != "" {
		fdbDB, err = fdb.OpenDatabase(clusterFile)
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
	existingMeters, _, err := db.Meters().List(context.Background(), 0, nil)
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

	// Health check (liveness) — always 200 if the process is up
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
			// Read the record count to verify FDB is reachable
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

	// Register all services
	register := func(path string, handler http.Handler) {
		mux.Handle(path, handler)
	}

	register(metrognomev1connect.NewCustomerServiceHandler(services.NewCustomerService(db.Customers())))
	register(metrognomev1connect.NewMeterServiceHandler(services.NewMeterService(db.Meters(), meterEngine)))
	register(metrognomev1connect.NewPlanServiceHandler(services.NewPlanService(db.Plans(), db.Charges())))
	register(metrognomev1connect.NewContractServiceHandler(services.NewContractService(db.Contracts())))
	register(metrognomev1connect.NewEventServiceHandler(services.NewEventService(db.Events(), db.Alerts(), meterEngine)))
	register(metrognomev1connect.NewInvoiceServiceHandler(services.NewInvoiceService(db.Invoices(), billingEngine)))
	register(metrognomev1connect.NewCreditServiceHandler(services.NewCreditService(db.Credits())))
	register(metrognomev1connect.NewAlertServiceHandler(services.NewAlertService(db.Alerts())))

	// CORS for frontend
	frontendURL := os.Getenv("FRONTEND_URL")
	if frontendURL == "" {
		frontendURL = "http://localhost:3000"
	}
	handler := services.CORSMiddleware(frontendURL, mux)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start Kafka consumer if brokers configured
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	kafkaTopic := os.Getenv("KAFKA_TOPIC")
	consumerCtx, consumerCancel := context.WithCancel(context.Background())
	defer consumerCancel()
	if kafkaBrokers != "" && kafkaTopic != "" {
		var err error
		kafkaConsumer, err = consumer.New(consumer.Config{
			Brokers: []string{kafkaBrokers},
			Topic:   kafkaTopic,
			GroupID: "metrognome",
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
		slog.Info("kafka consumer started", "brokers", kafkaBrokers, "topic", kafkaTopic)
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		slog.Info("metrognome starting", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := <-done
	slog.Info("shutting down", "signal", sig)
	consumerCancel() // stop kafka consumer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	slog.Info("metrognome stopped")
}
