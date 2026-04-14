package main

import (
	"context"
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

	// Set up HTTP mux with ConnectRPC services
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok","service":"metrognome"}`)
	})

	// Register all services
	register := func(path string, handler http.Handler) {
		mux.Handle(path, handler)
	}

	register(metrognomev1connect.NewCustomerServiceHandler(services.NewCustomerService(db.Customers())))
	register(metrognomev1connect.NewMeterServiceHandler(services.NewMeterService(db.Meters(), meterEngine)))
	register(metrognomev1connect.NewPlanServiceHandler(services.NewPlanService(db.Plans(), db.Charges())))
	register(metrognomev1connect.NewContractServiceHandler(services.NewContractService(db.Contracts())))
	register(metrognomev1connect.NewEventServiceHandler(services.NewEventService(db.Events(), meterEngine)))
	register(metrognomev1connect.NewInvoiceServiceHandler(services.NewInvoiceService(db.Invoices(), billingEngine)))
	register(metrognomev1connect.NewCreditServiceHandler(services.NewCreditService(db.Credits())))
	register(metrognomev1connect.NewAlertServiceHandler(services.NewAlertService(db.Alerts())))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start Kafka consumer if brokers configured
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	kafkaTopic := os.Getenv("KAFKA_TOPIC")
	consumerCtx, consumerCancel := context.WithCancel(context.Background())
	defer consumerCancel()
	if kafkaBrokers != "" && kafkaTopic != "" {
		kafkaConsumer, err := consumer.New(consumer.Config{
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
