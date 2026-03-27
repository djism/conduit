package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/djism/conduit/config"
	"github.com/djism/conduit/internal/cache"
	"github.com/djism/conduit/internal/db"
	"github.com/djism/conduit/internal/dlq"
	"github.com/djism/conduit/internal/graph"
	"github.com/djism/conduit/internal/kafka"
	"github.com/djism/conduit/internal/lag"
	"github.com/djism/conduit/internal/server"

	"github.com/joho/godotenv"
)

func main() {
	// ── Load environment variables ────────────────────────────────────────
	// godotenv loads .env into os.Environ so config.Load() can read them.
	// In production (Docker/K8s), env vars are injected directly —
	// godotenv is a no-op if .env doesn't exist, which is correct behavior.
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found — using environment variables directly")
	}

	// ── Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	log.Printf("conduit starting in %s mode", cfg.Env)

	// ── PostgreSQL ────────────────────────────────────────────────────────
	// Connects and runs all migrations before anything else starts.
	// If this fails, nothing else can work — crash immediately.
	database, err := db.New(cfg)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer database.Close()

	// ── Redis ─────────────────────────────────────────────────────────────
	cacheClient, err := cache.New(cfg)
	if err != nil {
		log.Fatalf("failed to connect to redis: %v", err)
	}
	defer cacheClient.Close()

	// ── Kafka Admin ───────────────────────────────────────────────────────
	// Admin client for metadata queries and lag computation.
	admin := kafka.NewAdminClient(cfg)

	// ── Schema Registry ───────────────────────────────────────────────────
	schemaRegistry := kafka.NewSchemaRegistry(database)

	// ── DLQ Store ─────────────────────────────────────────────────────────
	dlqStore := dlq.New(database)

	// ── Producer ──────────────────────────────────────────────────────────
	// Producer depends on schema registry and DLQ store —
	// both must be initialized before the producer starts.
	producer := kafka.NewProducer(cfg, schemaRegistry, dlqStore)
	defer producer.Close()

	// ── Lag Tracker ───────────────────────────────────────────────────────
	// Start the background polling loop.
	// Uses a background context — the tracker runs until Stop() is called.
	tracker := lag.New(cfg, admin, cacheClient)
	trackerCtx, trackerCancel := context.WithCancel(context.Background())
	defer trackerCancel()
	tracker.Start(trackerCtx)

	// ── GraphQL Resolver ──────────────────────────────────────────────────
	// Inject all services into the resolver.
	// Every GraphQL operation accesses services through this struct.
	resolver := &graph.Resolver{
		Config:   cfg,
		DLQ:      dlqStore,
		Schemas:  schemaRegistry,
		Tracker:  tracker,
		Admin:    admin,
		Producer: producer,
	}

	// ── HTTP Server ───────────────────────────────────────────────────────
	httpServer := server.New(cfg, database, cacheClient, resolver)

	// ── Graceful Shutdown ─────────────────────────────────────────────────
	// Listen for SIGINT (Ctrl+C) and SIGTERM (Docker/K8s stop).
	// When received:
	//  1. Stop accepting new connections
	//  2. Drain in-flight requests (up to 15s)
	//  3. Stop lag tracker
	//  4. Close database connections
	//  5. Exit cleanly
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP server in a goroutine so it doesn't block
	// the signal handler below.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- httpServer.Start()
	}()

	// Block until either the server errors or a shutdown signal arrives.
	select {
	case err := <-serverErr:
		log.Fatalf("server error: %v", err)

	case sig := <-stop:
		log.Printf("received signal %v — shutting down gracefully", sig)

		// Give in-flight requests 15 seconds to complete.
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.Background(),
			15*time.Second,
		)
		defer shutdownCancel()

		// Stop the lag tracker first — no new subscriptions.
		tracker.Stop()

		// Drain the HTTP server.
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown error: %v", err)
		}

		log.Println("conduit stopped cleanly")
	}
}
