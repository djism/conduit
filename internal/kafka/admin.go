package kafka

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/djism/conduit/config"
	kafkago "github.com/segmentio/kafka-go"
)

// PartitionLag holds the lag measurement for a single partition
// within a consumer group. Lag is tracked per-partition because
// Kafka parallelism is partition-based — a slow partition in an
// otherwise healthy consumer group is a common failure mode.
type PartitionLag struct {
	Topic           string
	Partition       int
	ConsumerGroup   string
	LatestOffset    int64 // how far the producer has written
	CommittedOffset int64 // how far the consumer has confirmed
	Lag             int64 // LatestOffset - CommittedOffset
}

// TopicInfo holds metadata about a Kafka topic fetched from the broker.
// Stored in PostgreSQL and surfaced on the dashboard topology.
type TopicInfo struct {
	Name              string
	Partitions        int
	ReplicationFactor int
}

// AdminClient wraps kafka-go's reader/dialer to provide
// Conduit-specific admin operations.
//
// kafka-go doesn't have a dedicated Admin client like the Java SDK —
// admin operations go through the Conn type which connects directly
// to a broker. We wrap this to keep Kafka internals out of other packages.
type AdminClient struct {
	cfg     *config.Config
	brokers []string
}

// NewAdminClient creates an AdminClient.
// Does not connect immediately — connections are made per-operation
// because Kafka admin operations are infrequent and short-lived.
// A persistent admin connection would hold a broker socket open for
// no benefit.
func NewAdminClient(cfg *config.Config) *AdminClient {
	return &AdminClient{
		cfg:     cfg,
		brokers: cfg.KafkaBrokers,
	}
}

// GetTopicMetadata fetches partition count and leader information
// for all topics Conduit manages.
//
// How it works:
//  1. Dial the first broker (the coordinator for metadata requests)
//  2. Call ReadPartitions — returns one entry per partition per topic
//  3. Aggregate partition entries by topic to get counts
//
// Called at startup to populate the topics table and on dashboard
// topology requests to show current partition layout.
func (a *AdminClient) GetTopicMetadata(ctx context.Context) ([]TopicInfo, error) {
	conn, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	infos := make([]TopicInfo, 0, len(a.cfg.KafkaTopics))

	for _, topic := range a.cfg.KafkaTopics {
		// ReadPartitions returns one kafkago.Partition per partition.
		// We count them to get the total partition count.
		partitions, err := conn.ReadPartitions(topic)
		if err != nil {
			// Log and continue — a missing topic shouldn't crash startup.
			// The topic might not exist yet; the producer will create it.
			log.Printf("warn: could not read partitions for topic %q: %v", topic, err)
			continue
		}

		// Replication factor is the number of replicas for partition 0.
		// All partitions in a topic have the same replication factor.
		replicationFactor := 1
		if len(partitions) > 0 {
			replicationFactor = len(partitions[0].Replicas)
		}

		infos = append(infos, TopicInfo{
			Name:              topic,
			Partitions:        len(partitions),
			ReplicationFactor: replicationFactor,
		})
	}

	return infos, nil
}

// GetLatestOffsets returns the latest (end) offset for every partition
// of a given topic.
//
// The latest offset is the offset of the next message to be written —
// i.e. the total number of messages ever written to that partition.
// We use this as the "producer position" in the lag calculation.
//
// Implementation:
//
//	We open a temporary reader positioned at LastOffset for each partition,
//	then read its current offset. No messages are consumed — we just
//	inspect the offset position.
func (a *AdminClient) GetLatestOffsets(ctx context.Context, topic string) (map[int]int64, error) {
	conn, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	partitions, err := conn.ReadPartitions(topic)
	if err != nil {
		return nil, fmt.Errorf("reading partitions for %s: %w", topic, err)
	}

	offsets := make(map[int]int64, len(partitions))

	for _, p := range partitions {
		// Open a reader at the last offset to get the current end position.
		// DialLeader connects to the partition leader directly —
		// only the leader knows the true latest offset.
		partitionConn, err := kafkago.DialLeader(
			ctx,
			"tcp",
			a.brokers[0],
			topic,
			p.ID,
		)
		if err != nil {
			log.Printf("warn: could not dial leader for %s partition %d: %v",
				topic, p.ID, err)
			continue
		}

		// ReadLastOffset returns the offset of the next message to be written.
		// This is what Kafka calls the "log end offset" (LEO).
		latestOffset, err := partitionConn.ReadLastOffset()
		partitionConn.Close()

		if err != nil {
			log.Printf("warn: could not read last offset for %s partition %d: %v",
				topic, p.ID, err)
			continue
		}

		offsets[p.ID] = latestOffset
	}

	return offsets, nil
}

// GetConsumerGroupOffsets returns the committed offset for every
// partition a consumer group is assigned to on a given topic.
//
// The committed offset is the last offset the consumer group
// successfully processed and acknowledged — "I've handled everything
// up to and including this message."
//
// kafka-go fetches committed offsets by opening a reader for the
// consumer group and inspecting its stored offset state.
func (a *AdminClient) GetConsumerGroupOffsets(
	ctx context.Context,
	topic, consumerGroup string,
) (map[int]int64, error) {
	conn, err := a.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	partitions, err := conn.ReadPartitions(topic)
	if err != nil {
		return nil, fmt.Errorf("reading partitions for %s: %w", topic, err)
	}

	offsets := make(map[int]int64, len(partitions))

	for _, p := range partitions {
		// kafka-go fetches committed offsets via a Reader configured
		// with the consumer group ID. The reader's Offset() method
		// returns the last committed offset for this partition.
		reader := kafkago.NewReader(kafkago.ReaderConfig{
			Brokers:   a.brokers,
			Topic:     topic,
			Partition: p.ID,
			GroupID:   consumerGroup,
			// MinBytes/MaxBytes control fetch size.
			// Small values here because we're just reading metadata.
			MinBytes: 1,
			MaxBytes: 1,
		})

		// FetchMessage with a short timeout gives us the current
		// reader position without consuming any messages.
		// We use the reader's offset as the committed offset.
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		committedOffset, err := reader.FetchMessage(fetchCtx)
		cancel()
		reader.Close()

		if err != nil {
			// No committed offset yet — consumer hasn't started.
			// Default to 0 (beginning of partition).
			offsets[p.ID] = 0
			continue
		}

		offsets[p.ID] = committedOffset.Offset
	}

	return offsets, nil
}

// ComputeLag calculates per-partition lag for a consumer group on a topic.
//
// This is the core of what makes Conduit useful — the lag number is
// the single most important health metric for a Kafka consumer.
//
// Algorithm:
//  1. Get latest offsets (producer position) for all partitions
//  2. Get committed offsets (consumer position) for all partitions
//  3. Lag = latest - committed, per partition
//  4. Return one PartitionLag per partition
func (a *AdminClient) ComputeLag(
	ctx context.Context,
	topic, consumerGroup string,
) ([]PartitionLag, error) {
	// Fetch both offset maps concurrently.
	// They're independent calls — no reason to run them sequentially.
	type result struct {
		offsets map[int]int64
		err     error
	}

	latestCh := make(chan result, 1)
	committedCh := make(chan result, 1)

	go func() {
		offsets, err := a.GetLatestOffsets(ctx, topic)
		latestCh <- result{offsets, err}
	}()

	go func() {
		offsets, err := a.GetConsumerGroupOffsets(ctx, topic, consumerGroup)
		committedCh <- result{offsets, err}
	}()

	latestResult := <-latestCh
	if latestResult.err != nil {
		return nil, fmt.Errorf("getting latest offsets: %w", latestResult.err)
	}

	committedResult := <-committedCh
	if committedResult.err != nil {
		return nil, fmt.Errorf("getting committed offsets: %w", committedResult.err)
	}

	latestOffsets := latestResult.offsets
	committedOffsets := committedResult.offsets

	// Compute lag per partition.
	lags := make([]PartitionLag, 0, len(latestOffsets))
	for partitionID, latestOffset := range latestOffsets {
		committedOffset := committedOffsets[partitionID] // 0 if not found

		lag := latestOffset - committedOffset
		if lag < 0 {
			// Can happen briefly during rebalance — clamp to 0.
			lag = 0
		}

		lags = append(lags, PartitionLag{
			Topic:           topic,
			Partition:       partitionID,
			ConsumerGroup:   consumerGroup,
			LatestOffset:    latestOffset,
			CommittedOffset: committedOffset,
			Lag:             lag,
		})
	}

	return lags, nil
}

// HealthCheck verifies Conduit can reach at least one Kafka broker.
// Called by the /health endpoint.
func (a *AdminClient) HealthCheck(ctx context.Context) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return fmt.Errorf("kafka health check failed: %w", err)
	}
	conn.Close()
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// dial opens a connection to the first available Kafka broker.
// Uses a 5-second timeout so a dead broker doesn't hang the caller.
//
// We always connect to brokers[0] for admin operations.
// For production you'd add retry logic across all brokers,
// but for Conduit's use case (single local cluster) this is sufficient.
func (a *AdminClient) dial(ctx context.Context) (*kafkago.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := kafkago.DialContext(dialCtx, "tcp", a.brokers[0])
	if err != nil {
		return nil, fmt.Errorf("dialing kafka broker %s: %w", a.brokers[0], err)
	}

	return conn, nil
}
