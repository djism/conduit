package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/djism/conduit/config"
	"github.com/djism/conduit/internal/cache"
	"github.com/djism/conduit/internal/db"
	"github.com/djism/conduit/internal/graph"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	generated "github.com/djism/conduit/internal/graph"
)

// Server holds the HTTP server and all its dependencies.
// Created once in main.go and started with ListenAndServe.
type Server struct {
	cfg      *config.Config
	db       *db.DB
	cache    *cache.Client
	resolver *graph.Resolver
	httpSrv  *http.Server
}

// New creates the HTTP server, registers all routes, and returns
// it ready to start. Does not bind to a port yet — call Start().
func New(
	cfg *config.Config,
	database *db.DB,
	cacheClient *cache.Client,
	resolver *graph.Resolver,
) *Server {
	mux := http.NewServeMux()

	srv := &Server{
		cfg:      cfg,
		db:       database,
		cache:    cacheClient,
		resolver: resolver,
	}

	// ── GraphQL ───────────────────────────────────────────────────────────
	// gqlgen's server handler — handles both HTTP queries/mutations
	// and WebSocket subscription upgrades on the same endpoint.
	gqlHandler := handler.New(
		generated.NewExecutableSchema(
			generated.Config{Resolvers: resolver},
		),
	)

	// Transports define how clients can communicate with the server.
	// Order matters — gqlgen tries each transport in order.
	gqlHandler.AddTransport(transport.Websocket{
		// WebSocket upgrader — allows browser clients to connect
		// for GraphQL subscriptions (live lag updates).
		Upgrader: websocket.Upgrader{
			// CheckOrigin always returns true in development.
			// In production you'd validate the Origin header
			// against an allowlist.
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			HandshakeTimeout: 10 * time.Second,
		},
		// KeepAlivePingInterval sends a ping frame every 10s
		// to prevent proxies from closing idle WebSocket connections.
		KeepAlivePingInterval: 10 * time.Second,
	})
	gqlHandler.AddTransport(transport.Options{})
	gqlHandler.AddTransport(transport.GET{})
	gqlHandler.AddTransport(transport.POST{})
	gqlHandler.AddTransport(transport.MultipartForm{})

	// Enable introspection so the GraphQL Playground works.
	// In production you'd disable this.
	gqlHandler.Use(extension.Introspection{})

	// ── Routes ────────────────────────────────────────────────────────────
	// /query      — GraphQL endpoint (queries, mutations, subscriptions)
	// /playground — GraphQL Playground IDE (development only)
	// /health     — Health check (Kafka + PostgreSQL + Redis)
	// /metrics    — Prometheus metrics scrape endpoint
	mux.Handle("/query", srv.corsMiddleware(gqlHandler))
	mux.Handle("/playground", playground.Handler("Conduit GraphQL", "/query"))
	mux.HandleFunc("/health", srv.healthHandler)
	mux.Handle("/metrics", promhttp.Handler())

	srv.httpSrv = &http.Server{
		Addr:    ":" + cfg.ServerPort,
		Handler: mux,

		// Timeouts prevent slow clients from holding connections open.
		// ReadTimeout:  max time to read the full request
		// WriteTimeout: max time to write the full response
		// IdleTimeout:  max time to keep an idle connection alive
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	return srv
}

// Start binds to the configured port and starts serving requests.
// Blocks until the server shuts down.
// Call Shutdown() from another goroutine to stop it gracefully.
func (s *Server) Start() error {
	log.Printf("✓ HTTP server listening on :%s", s.cfg.ServerPort)
	log.Printf("  GraphQL:    http://localhost:%s/query", s.cfg.ServerPort)
	log.Printf("  Playground: http://localhost:%s/playground", s.cfg.ServerPort)
	log.Printf("  Health:     http://localhost:%s/health", s.cfg.ServerPort)
	log.Printf("  Metrics:    http://localhost:%s/metrics", s.cfg.ServerPort)

	if err := s.httpSrv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully drains in-flight requests before stopping.
// Gives connections up to 10 seconds to finish.
// Called on SIGINT/SIGTERM in main.go.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Println("HTTP server shutting down...")
	return s.httpSrv.Shutdown(ctx)
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// healthHandler checks all dependencies and returns 200 OK if healthy,
// 503 Service Unavailable if any dependency is down.
//
// Response body:
//
//	{
//	  "status": "ok",
//	  "postgres": "ok",
//	  "redis": "ok",
//	  "kafka": "ok"
//	}
//
// Load balancers poll this endpoint to decide whether to route
// traffic to this instance.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	health := map[string]string{
		"status":   "ok",
		"postgres": "ok",
		"redis":    "ok",
		"kafka":    "ok",
	}

	statusCode := http.StatusOK

	if err := s.db.HealthCheck(); err != nil {
		health["postgres"] = err.Error()
		health["status"] = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	if err := s.cache.HealthCheck(ctx); err != nil {
		health["redis"] = err.Error()
		health["status"] = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	if err := s.resolver.Admin.HealthCheck(ctx); err != nil {
		health["kafka"] = err.Error()
		health["status"] = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(health)
}

// ── Middleware ─────────────────────────────────────────────────────────────────

// corsMiddleware adds CORS headers to allow the React dashboard
// (running on localhost:5173 in development) to call the API.
//
// Headers set:
//   - Access-Control-Allow-Origin: * (dev) or specific origin (prod)
//   - Access-Control-Allow-Methods: GET, POST, OPTIONS
//   - Access-Control-Allow-Headers: includes Authorization for JWT
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In development, allow all origins.
		// In production, restrict to your actual frontend domain.
		origin := "*"
		if !s.cfg.IsDevelopment() {
			origin = "https://your-production-domain.com"
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods",
			"GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, Authorization, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		// Preflight request — browser sends OPTIONS before the real request.
		// Return 200 immediately so the browser proceeds with the real request.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
