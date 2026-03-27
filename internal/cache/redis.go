package cache

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/djism/conduit/config"
	"github.com/redis/go-redis/v9"
)

// Client wraps redis.Client to add Conduit-specific methods.
// All lag time-series operations live here — nothing else in the
// codebase touches Redis directly except through this struct.
type Client struct {
	rdb *redis.Client
	cfg *config.Config
}

// LagPoint represents a single consumer lag measurement at a point in time.
// Used to populate the dashboard time-series chart.
type LagPoint struct {
	Timestamp time.Time
	Lag       int64
}

// New creates a Redis client and verifies the connection with a PING.
// Returns an error immediately if Redis is unreachable — fail fast at startup.
func New(cfg *config.Config) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr(),
		Password: cfg.RedisPassword,
		DB:       0, // default DB

		// Connection pool settings.
		// PoolSize controls how many simultaneous Redis connections are open.
		// For a moderate-traffic service, 10 is sufficient.
		// The lag tracker writes every 5 seconds per consumer group —
		// not a high-throughput workload.
		PoolSize:    10,
		DialTimeout: 5 * time.Second,
		ReadTimeout: 3 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// PING verifies the connection is actually alive.
	// Without this, a misconfigured Redis address would fail silently
	// until the first real operation.
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("pinging redis: %w", err)
	}

	log.Println("✓ Redis connected")
	return &Client{rdb: rdb, cfg: cfg}, nil
}

// ── Lag Time-Series ──────────────────────────────────────────────────────────

// StoreLag records a consumer lag reading into a Redis Sorted Set.
//
// Data structure:
//
//	Key:    conduit:lag:{topic}:{consumerGroup}
//	Score:  Unix timestamp (seconds) — enables time-range queries
//	Member: "{timestamp}:{lag}" — stores both values in the member string
//
// Why Sorted Sets:
//
//	ZRANGEBYSCORE gives O(log n) time-range queries.
//	"Give me all readings in the last 30 minutes" = one Redis call.
//	A List would require O(n) scan; a Hash can't do range queries at all.
//
// The key has a TTL equal to LagHistoryTTLHours from config (default 24h).
// Redis evicts the entire key after TTL — no manual cleanup needed.
func (c *Client) StoreLag(ctx context.Context, topic, consumerGroup string, lag int64) error {
	key := lagKey(topic, consumerGroup)
	now := time.Now()

	// Member encodes both timestamp and lag so we can decode both on read.
	// Format: "1711234567:4521"
	member := fmt.Sprintf("%d:%d", now.Unix(), lag)

	// ZADD adds the member with score = unix timestamp.
	// If the same member already exists, it updates the score.
	if err := c.rdb.ZAdd(ctx, key, redis.Z{
		Score:  float64(now.Unix()),
		Member: member,
	}).Err(); err != nil {
		return fmt.Errorf("storing lag for %s/%s: %w", topic, consumerGroup, err)
	}

	// Reset TTL on every write so the key lives for TTL hours
	// from the last write, not from the first write.
	ttl := time.Duration(c.cfg.LagHistoryTTLHours) * time.Hour
	if err := c.rdb.Expire(ctx, key, ttl).Err(); err != nil {
		// Non-fatal — lag data is still stored, TTL just won't refresh.
		log.Printf("warn: failed to set TTL for lag key %s: %v", key, err)
	}

	return nil
}

// GetLagHistory returns all lag readings for a topic+consumerGroup
// within the specified time window.
//
// Uses ZRANGEBYSCORE to query by timestamp range — O(log n + k)
// where k is the number of results. Fast even with millions of readings.
//
// Returns points sorted oldest-first (ascending timestamp) —
// the format Recharts expects for time-series charts.
func (c *Client) GetLagHistory(
	ctx context.Context,
	topic, consumerGroup string,
	from, to time.Time,
) ([]LagPoint, error) {
	key := lagKey(topic, consumerGroup)

	// ZRANGEBYSCORE returns members with score between from and to.
	// Scores are unix timestamps, so this is a direct time-range query.
	members, err := c.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: strconv.FormatInt(from.Unix(), 10),
		Max: strconv.FormatInt(to.Unix(), 10),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("querying lag history for %s/%s: %w", topic, consumerGroup, err)
	}

	points := make([]LagPoint, 0, len(members))
	for _, member := range members {
		point, err := parseLagMember(member)
		if err != nil {
			// Skip malformed entries rather than failing the whole query.
			log.Printf("warn: skipping malformed lag member %q: %v", member, err)
			continue
		}
		points = append(points, point)
	}

	return points, nil
}

// GetLatestLag returns the single most recent lag reading for a
// topic + consumer group pair. Used by the dashboard summary cards
// that show current lag (not historical).
//
// ZREVRANGEBYSCORE with LIMIT 1 fetches the highest-scored (most recent)
// member in O(log n) time.
func (c *Client) GetLatestLag(ctx context.Context, topic, consumerGroup string) (int64, error) {
	key := lagKey(topic, consumerGroup)

	// ZREVRANGE returns members in reverse score order (highest first).
	// We only want the single most recent reading.
	members, err := c.rdb.ZRevRange(ctx, key, 0, 0).Result()
	if err != nil {
		return 0, fmt.Errorf("getting latest lag for %s/%s: %w", topic, consumerGroup, err)
	}

	if len(members) == 0 {
		return 0, nil // no data yet
	}

	point, err := parseLagMember(members[0])
	if err != nil {
		return 0, fmt.Errorf("parsing latest lag member: %w", err)
	}

	return point.Lag, nil
}

// PruneLagHistory removes lag readings older than the configured TTL.
//
// This is belt-and-suspenders cleanup — the key-level TTL handles
// eviction automatically, but ZREMRANGEBYSCORE lets us prune individual
// old members without evicting the entire key.
// Called by the lag tracker after each write cycle.
func (c *Client) PruneLagHistory(ctx context.Context, topic, consumerGroup string) error {
	key := lagKey(topic, consumerGroup)
	cutoff := time.Now().Add(-time.Duration(c.cfg.LagHistoryTTLHours) * time.Hour)

	// ZREMRANGEBYSCORE removes all members with score below cutoff.
	// "-inf" means "from the beginning" — remove everything older than cutoff.
	if err := c.rdb.ZRemRangeByScore(ctx, key,
		"-inf",
		strconv.FormatInt(cutoff.Unix(), 10),
	).Err(); err != nil {
		return fmt.Errorf("pruning lag history for %s/%s: %w", topic, consumerGroup, err)
	}

	return nil
}

// ── Health + Lifecycle ───────────────────────────────────────────────────────

// HealthCheck verifies Redis is still reachable.
// Called by the /health endpoint.
func (c *Client) HealthCheck(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis health check failed: %w", err)
	}
	return nil
}

// Close gracefully shuts down the Redis connection pool.
// Always call this on server shutdown to avoid connection leaks.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// lagKey builds the Redis key for a topic + consumer group combination.
// Format: conduit:lag:{topic}:{consumerGroup}
//
// Namespace prefix "conduit:" isolates our keys from anything else
// using the same Redis instance — a production best practice.
func lagKey(topic, consumerGroup string) string {
	return fmt.Sprintf("conduit:lag:%s:%s", topic, consumerGroup)
}

// parseLagMember decodes a member string back into a LagPoint.
// Member format: "{unixTimestamp}:{lagValue}"
// Example: "1711234567:4521" → LagPoint{Timestamp: ..., Lag: 4521}
func parseLagMember(member string) (LagPoint, error) {
	parts := strings.SplitN(member, ":", 2)
	if len(parts) != 2 {
		return LagPoint{}, fmt.Errorf("expected format timestamp:lag, got %q", member)
	}

	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return LagPoint{}, fmt.Errorf("parsing timestamp %q: %w", parts[0], err)
	}

	lag, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return LagPoint{}, fmt.Errorf("parsing lag %q: %w", parts[1], err)
	}

	return LagPoint{
		Timestamp: time.Unix(ts, 0),
		Lag:       lag,
	}, nil
}
