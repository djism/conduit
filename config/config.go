package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for Conduit.
// Loaded once at startup in cmd/server/main.go.
// Every field maps directly to a .env variable.
// No package outside config ever calls os.Getenv directly —
// this keeps configuration changes in one place.
type Config struct {
	// Server
	ServerPort string
	Env        string

	// Kafka
	KafkaBrokers []string // e.g. ["localhost:9092"]
	KafkaGroupID string
	KafkaTopics  []string // topics Conduit manages

	// PostgreSQL — stores schemas, DLQ events, topic metadata
	PostgresHost     string
	PostgresPort     int
	PostgresUser     string
	PostgresPassword string
	PostgresDB       string

	// Redis — consumer lag time-series, query cache
	RedisHost     string
	RedisPort     int
	RedisPassword string

	// Lag tracking
	LagPollIntervalSeconds int
	LagHistoryTTLHours     int
	LagAlertThreshold      int64 // messages behind before alert fires

	// Schema validation
	SchemaValidationEnabled bool

	// Observability
	PrometheusPort string
}

// Load reads all environment variables and returns a validated Config.
// Panics immediately on missing required variables — better to crash
// at startup with a clear message than fail silently at runtime.
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort: getEnvOrDefault("SERVER_PORT", "8080"),
		Env:        getEnvOrDefault("ENV", "development"),

		KafkaBrokers: parseCSV(requireEnv("KAFKA_BROKERS")),
		KafkaGroupID: requireEnv("KAFKA_GROUP_ID"),
		KafkaTopics:  parseCSV(requireEnv("KAFKA_TOPICS")),

		PostgresHost:     requireEnv("POSTGRES_HOST"),
		PostgresPort:     requireInt("POSTGRES_PORT"),
		PostgresUser:     requireEnv("POSTGRES_USER"),
		PostgresPassword: requireEnv("POSTGRES_PASSWORD"),
		PostgresDB:       requireEnv("POSTGRES_DB"),

		RedisHost:     requireEnv("REDIS_HOST"),
		RedisPort:     requireInt("REDIS_PORT"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"), // optional

		LagPollIntervalSeconds: requireInt("LAG_POLL_INTERVAL_SECONDS"),
		LagHistoryTTLHours:     requireInt("LAG_HISTORY_TTL_HOURS"),
		LagAlertThreshold:      int64(requireInt("LAG_ALERT_THRESHOLD")),

		SchemaValidationEnabled: getEnvBool("SCHEMA_VALIDATION_ENABLED", true),

		PrometheusPort: getEnvOrDefault("PROMETHEUS_PORT", "9090"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// PostgresDSN returns the connection string for lib/pq.
func (c *Config) PostgresDSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		c.PostgresHost, c.PostgresPort,
		c.PostgresUser, c.PostgresPassword,
		c.PostgresDB,
	)
}

// RedisAddr returns host:port for the Redis client.
func (c *Config) RedisAddr() string {
	return fmt.Sprintf("%s:%d", c.RedisHost, c.RedisPort)
}

// IsDevelopment returns true when running locally.
// Used to enable debug logging and relaxed CORS.
func (c *Config) IsDevelopment() bool {
	return c.Env == "development"
}

// validate encodes domain constraints between config values.
// Catches misconfiguration at startup before any damage is done.
func (c *Config) validate() error {
	if len(c.KafkaBrokers) == 0 {
		return fmt.Errorf("KAFKA_BROKERS must contain at least one broker")
	}
	if len(c.KafkaTopics) == 0 {
		return fmt.Errorf("KAFKA_TOPICS must contain at least one topic")
	}
	if c.LagPollIntervalSeconds < 1 {
		return fmt.Errorf("LAG_POLL_INTERVAL_SECONDS must be at least 1")
	}
	if c.LagAlertThreshold < 0 {
		return fmt.Errorf("LAG_ALERT_THRESHOLD must be non-negative")
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}

func requireInt(key string) int {
	v := requireEnv(key)
	n, err := strconv.Atoi(v)
	if err != nil {
		panic(fmt.Sprintf("environment variable %q must be an integer, got %q", key, v))
	}
	return n
}

func getEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

// parseCSV splits a comma-separated string into a trimmed slice.
// "localhost:9092, localhost:9093" → ["localhost:9092", "localhost:9093"]
func parseCSV(raw string) []string {
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
