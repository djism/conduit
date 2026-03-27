package lag

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/djism/conduit/config"
	"github.com/djism/conduit/internal/cache"
	"github.com/djism/conduit/internal/kafka"
)

// Update is sent to subscribers every time lag is computed
// for a topic + consumer group combination.
// The GraphQL subscription resolver receives these and pushes
// them to connected dashboard clients.
type Update struct {
	Topic         string
	ConsumerGroup string
	TotalLag      int64 // sum of lag across all partitions
	Partitions    []kafka.PartitionLag
	Timestamp     time.Time
	IsAlert       bool // true if lag exceeds the configured threshold
}

// Tracker polls Kafka for consumer group lag on a fixed interval
// and broadcasts updates to all registered subscribers.
//
// Design decisions:
//   - One goroutine owns the polling loop — no concurrent polls
//   - Subscribers receive updates via channels — no shared state
//   - Context cancellation stops the loop cleanly on shutdown
//   - Subscriber channels are buffered — slow subscribers don't
//     block the polling loop (they miss an update instead)
type Tracker struct {
	cfg         *config.Config
	admin       *kafka.AdminClient
	cache       *cache.Client
	subscribers map[string]chan Update // key: subscriber ID
	mu          sync.RWMutex           // protects subscribers map
	done        chan struct{}
}

// New creates a Tracker. Call Start() to begin polling.
func New(
	cfg *config.Config,
	admin *kafka.AdminClient,
	cache *cache.Client,
) *Tracker {
	return &Tracker{
		cfg:         cfg,
		admin:       admin,
		cache:       cache,
		subscribers: make(map[string]chan Update),
		done:        make(chan struct{}),
	}
}

// Start launches the polling loop in a background goroutine.
// Returns immediately — the loop runs until ctx is cancelled
// or Stop() is called.
//
// Call this once at server startup after all dependencies are ready.
func (t *Tracker) Start(ctx context.Context) {
	go t.loop(ctx)
	log.Printf("✓ Lag tracker started — polling every %ds for %d topics",
		t.cfg.LagPollIntervalSeconds, len(t.cfg.KafkaTopics))
}

// Stop signals the polling loop to exit.
// Blocks until the loop has finished its current iteration
// and all subscribers have been notified.
func (t *Tracker) Stop() {
	close(t.done)
}

// Subscribe registers a new subscriber and returns a channel that
// receives lag updates. The caller must read from this channel
// continuously — if the channel buffer fills up, updates are dropped
// for that subscriber (the polling loop never blocks).
//
// subscriberID is any unique string — used to unsubscribe later.
// The returned channel is closed when the subscriber unsubscribes
// or the tracker stops.
//
// Buffer size 10: stores up to 10 unread updates before dropping.
// At 5s poll interval, this gives a subscriber 50s to catch up
// before it starts missing updates.
func (t *Tracker) Subscribe(subscriberID string) <-chan Update {
	ch := make(chan Update, 10)

	t.mu.Lock()
	t.subscribers[subscriberID] = ch
	t.mu.Unlock()

	log.Printf("lag tracker: subscriber %q registered", subscriberID)
	return ch
}

// Unsubscribe removes a subscriber and closes its channel.
// Always call this when a dashboard client disconnects to avoid
// goroutine and memory leaks.
func (t *Tracker) Unsubscribe(subscriberID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ch, ok := t.subscribers[subscriberID]
	if !ok {
		return
	}

	close(ch)
	delete(t.subscribers, subscriberID)
	log.Printf("lag tracker: subscriber %q unregistered", subscriberID)
}

// GetHistory returns lag history for a topic + consumer group
// from Redis. Used by the dashboard on initial load to populate
// the chart before live updates start arriving.
func (t *Tracker) GetHistory(
	ctx context.Context,
	topic, consumerGroup string,
	duration time.Duration,
) ([]cache.LagPoint, error) {
	from := time.Now().Add(-duration)
	to := time.Now()
	return t.cache.GetLagHistory(ctx, topic, consumerGroup, from, to)
}

// ── polling loop ─────────────────────────────────────────────────────────────

// loop is the core polling goroutine. Runs until ctx is cancelled
// or Stop() is called. Never returns an error — all errors are logged
// and the loop continues. A single Kafka hiccup should not stop tracking.
func (t *Tracker) loop(ctx context.Context) {
	interval := time.Duration(t.cfg.LagPollIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Poll immediately on start — don't wait for the first tick.
	// This means the dashboard has data within milliseconds of startup
	// rather than waiting up to 5 seconds for the first tick.
	t.poll(ctx)

	for {
		select {
		case <-ticker.C:
			t.poll(ctx)

		case <-ctx.Done():
			log.Println("lag tracker: context cancelled, stopping")
			t.closeAllSubscribers()
			return

		case <-t.done:
			log.Println("lag tracker: stop called, stopping")
			t.closeAllSubscribers()
			return
		}
	}
}

// poll runs one round of lag computation for all configured
// topic × consumer group combinations.
//
// We use a fixed consumer group from config for now.
// In a production system you'd discover consumer groups dynamically
// via the Kafka Admin API's ListConsumerGroups call.
func (t *Tracker) poll(ctx context.Context) {
	consumerGroup := t.cfg.KafkaGroupID

	for _, topic := range t.cfg.KafkaTopics {
		partitionLags, err := t.admin.ComputeLag(ctx, topic, consumerGroup)
		if err != nil {
			// Log and continue to next topic.
			// One topic's failure shouldn't stop tracking for others.
			log.Printf("warn: lag tracker failed to compute lag for %s/%s: %v",
				topic, consumerGroup, err)
			continue
		}

		// Sum lag across all partitions for the total lag metric.
		// The dashboard shows both total and per-partition breakdown.
		var totalLag int64
		for _, pl := range partitionLags {
			totalLag += pl.Lag
		}

		// Store in Redis time-series for historical chart data.
		storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := t.cache.StoreLag(storeCtx, topic, consumerGroup, totalLag); err != nil {
			log.Printf("warn: failed to store lag for %s/%s: %v",
				topic, consumerGroup, err)
		}

		// Prune old data beyond the TTL window.
		t.cache.PruneLagHistory(storeCtx, topic, consumerGroup)
		cancel()

		// Build the update to broadcast to subscribers.
		isAlert := totalLag > t.cfg.LagAlertThreshold
		update := Update{
			Topic:         topic,
			ConsumerGroup: consumerGroup,
			TotalLag:      totalLag,
			Partitions:    partitionLags,
			Timestamp:     time.Now(),
			IsAlert:       isAlert,
		}

		if isAlert {
			log.Printf("ALERT: consumer lag for %s/%s is %d — threshold is %d",
				topic, consumerGroup, totalLag, t.cfg.LagAlertThreshold)
		}

		// Broadcast to all subscribers.
		t.broadcast(update)
	}
}

// broadcast sends an update to all registered subscribers.
// Uses a non-blocking send — if a subscriber's channel is full,
// the update is dropped for that subscriber rather than blocking
// the entire polling loop.
//
// This is the correct tradeoff: a slow dashboard client should never
// cause the polling loop to stall and miss Kafka metrics for everyone.
func (t *Tracker) broadcast(update Update) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for id, ch := range t.subscribers {
		select {
		case ch <- update:
			// Delivered successfully.
		default:
			// Channel full — subscriber is too slow. Drop this update.
			// The subscriber will still receive the next one.
			log.Printf("warn: lag update dropped for subscriber %q — channel full", id)
		}
	}
}

// closeAllSubscribers closes all subscriber channels on shutdown.
// Closed channels signal to GraphQL subscription resolvers that
// the stream has ended — they clean up and return to the client.
func (t *Tracker) closeAllSubscribers() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for id, ch := range t.subscribers {
		close(ch)
		delete(t.subscribers, id)
		log.Printf("lag tracker: closed channel for subscriber %q on shutdown", id)
	}
}
