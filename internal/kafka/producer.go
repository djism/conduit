package kafka

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/djism/conduit/config"
	"github.com/google/uuid"
	kafkago "github.com/segmentio/kafka-go"
)

// PublishResult holds the outcome of a publish attempt.
// On success, Offset and Partition show where the message landed in Kafka.
// On failure, Error describes what went wrong and DLQEventID points to
// the DLQ entry created for this failed event.
type PublishResult struct {
	Success    bool
	Topic      string
	Partition  int
	Offset     int64
	DLQEventID string // set if event was routed to DLQ
	Error      string // human-readable failure reason
}

// Producer publishes events to Kafka topics with schema validation.
//
// Every publish call goes through this sequence:
//  1. Validate payload against the active schema for the topic
//  2. If invalid → write to DLQ, return failure result
//  3. If valid → publish to Kafka
//  4. If Kafka publish fails → write to DLQ, return failure result
//  5. If Kafka publish succeeds → return success result
//
// The DLQ store is injected so Producer never imports the dlq package
// directly — this avoids a circular dependency since dlq imports db
// and producer imports kafka. Dependency injection keeps the graph clean.
type Producer struct {
	cfg      *config.Config
	writer   *kafkago.Writer
	registry *SchemaRegistry
	dlqStore DLQStore // interface — see below
}

// DLQStore is the interface Producer uses to write failed events.
// Defined here as an interface so:
//  1. We avoid circular imports (producer → dlq → producer)
//  2. Tests can inject a mock DLQ store
//
// The real implementation lives in internal/dlq/store.go.
type DLQStore interface {
	Insert(ctx context.Context, event DLQEvent) (string, error)
}

// DLQEvent carries all the metadata needed to store a failed event.
// Matches the dead_letter_events table schema.
type DLQEvent struct {
	TopicName    string
	PartitionID  int
	KafkaOffset  int64
	Payload      []byte
	ErrorType    string
	ErrorMessage string
	ProducerID   string
}

// NewProducer creates a Producer with a kafka-go Writer configured
// for the given topic list.
//
// Writer configuration choices:
//   - Balancer: LeastBytes — routes messages to the partition with
//     least buffered data, giving roughly even distribution.
//     RoundRobin is simpler but ignores partition pressure.
//   - RequiredAcks: RequireAll — waits for all in-sync replicas to
//     acknowledge before returning. Slower than RequireOne but
//     guarantees no data loss if the leader fails immediately after write.
//   - Async: false — synchronous writes so we know immediately if
//     a message was accepted or failed. Async is faster but you lose
//     the ability to route failures to the DLQ inline.
func NewProducer(
	cfg *config.Config,
	registry *SchemaRegistry,
	dlqStore DLQStore,
) *Producer {
	writer := &kafkago.Writer{
		Addr:         kafkago.TCP(cfg.KafkaBrokers...),
		Balancer:     &kafkago.LeastBytes{},
		RequiredAcks: kafkago.RequireAll,
		Async:        false,

		// Retry configuration.
		// Up to 3 retries with exponential backoff before giving up.
		// This handles transient broker unavailability without
		// immediately routing to the DLQ.
		MaxAttempts: 3,

		// WriteTimeout caps how long we wait for broker acknowledgment.
		// 10 seconds is generous — in practice acks arrive in <100ms.
		WriteTimeout: 10 * time.Second,
	}

	return &Producer{
		cfg:      cfg,
		writer:   writer,
		registry: registry,
		dlqStore: dlqStore,
	}
}

// Publish validates and publishes a single event to a Kafka topic.
//
// producerID identifies the upstream service publishing this event —
// used in DLQ metadata to help debug which producer sent bad data.
// Pass an empty string if not applicable.
//
// Returns a PublishResult describing what happened.
// Never returns an error — all failures are encoded in PublishResult
// so callers don't need to handle both error paths.
func (p *Producer) Publish(
	ctx context.Context,
	topic string,
	key string,
	payload []byte,
	producerID string,
) PublishResult {
	// ── Step 1: Schema Validation ────────────────────────────────────────
	// Validate before touching Kafka. If the payload is invalid,
	// it goes directly to the DLQ — it never enters the topic.
	// This protects consumers from ever seeing malformed data.
	validationResult, err := p.registry.Validate(ctx, topic, payload)
	if err != nil {
		// System error (DB down, schema unreadable) — log and fail.
		// Don't route to DLQ for system errors — the event isn't malformed,
		// we just couldn't check it. In production you'd add a fallback policy.
		log.Printf("error: schema validation system error for topic %s: %v", topic, err)
		return PublishResult{
			Success: false,
			Topic:   topic,
			Error:   fmt.Sprintf("schema validation system error: %v", err),
		}
	}

	if !validationResult.IsValid {
		// Payload failed schema validation — route to DLQ immediately.
		// KafkaOffset = -1 signals the event never reached Kafka.
		errorMessage := formatValidationErrors(validationResult.Errors)
		dlqID := p.writeToDLQ(ctx, DLQEvent{
			TopicName:    topic,
			PartitionID:  0,
			KafkaOffset:  -1,
			Payload:      payload,
			ErrorType:    "schema_validation",
			ErrorMessage: errorMessage,
			ProducerID:   producerID,
		})

		log.Printf("event rejected for topic %s: schema validation failed: %s",
			topic, errorMessage)

		return PublishResult{
			Success:    false,
			Topic:      topic,
			DLQEventID: dlqID,
			Error:      errorMessage,
		}
	}

	// ── Step 2: Publish to Kafka ─────────────────────────────────────────
	// Payload is valid — write to the Kafka topic.
	// kafka-go's Writer handles retries, leader discovery, and batching.
	msg := kafkago.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: payload,

		// Headers carry metadata without modifying the payload.
		// These are visible to consumers and in the Conduit event explorer.
		Headers: []kafkago.Header{
			{Key: "conduit-producer-id", Value: []byte(producerID)},
			{Key: "conduit-schema-id", Value: []byte(validationResult.SchemaID)},
			{Key: "conduit-published-at", Value: []byte(time.Now().UTC().Format(time.RFC3339))},
		},
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		// Kafka write failed after retries — route to DLQ.
		// KafkaOffset = -1 because we don't know if it was partially written.
		errorMessage := fmt.Sprintf("kafka write failed: %v", err)
		dlqID := p.writeToDLQ(ctx, DLQEvent{
			TopicName:    topic,
			PartitionID:  0,
			KafkaOffset:  -1,
			Payload:      payload,
			ErrorType:    "publish_error",
			ErrorMessage: errorMessage,
			ProducerID:   producerID,
		})

		log.Printf("error: kafka write failed for topic %s: %v", topic, err)

		return PublishResult{
			Success:    false,
			Topic:      topic,
			DLQEventID: dlqID,
			Error:      errorMessage,
		}
	}

	return PublishResult{
		Success: true,
		Topic:   topic,
	}
}

// PublishBatch validates and publishes multiple events in a single
// Kafka write call. More efficient than individual Publish calls
// for high-throughput scenarios.
//
// Each event is validated individually — if any fail schema validation
// they go to the DLQ. Valid events are batched and written together.
// Returns one PublishResult per input event, in the same order.
func (p *Producer) PublishBatch(
	ctx context.Context,
	topic string,
	events []BatchEvent,
) []PublishResult {
	results := make([]PublishResult, len(events))
	validMessages := make([]kafkago.Message, 0, len(events))
	validIndices := make([]int, 0, len(events))

	// Validate each event individually.
	// Valid events are collected for batch write.
	// Invalid events are immediately routed to DLQ.
	for i, event := range events {
		validationResult, err := p.registry.Validate(ctx, topic, event.Payload)
		if err != nil {
			results[i] = PublishResult{
				Success: false,
				Topic:   topic,
				Error:   fmt.Sprintf("schema validation system error: %v", err),
			}
			continue
		}

		if !validationResult.IsValid {
			errorMessage := formatValidationErrors(validationResult.Errors)
			dlqID := p.writeToDLQ(ctx, DLQEvent{
				TopicName:    topic,
				KafkaOffset:  -1,
				Payload:      event.Payload,
				ErrorType:    "schema_validation",
				ErrorMessage: errorMessage,
				ProducerID:   event.ProducerID,
			})
			results[i] = PublishResult{
				Success:    false,
				Topic:      topic,
				DLQEventID: dlqID,
				Error:      errorMessage,
			}
			continue
		}

		// Valid — collect for batch write.
		validMessages = append(validMessages, kafkago.Message{
			Topic: topic,
			Key:   []byte(event.Key),
			Value: event.Payload,
			Headers: []kafkago.Header{
				{Key: "conduit-producer-id", Value: []byte(event.ProducerID)},
				{Key: "conduit-schema-id", Value: []byte(validationResult.SchemaID)},
			},
		})
		validIndices = append(validIndices, i)
	}

	// Write all valid messages in a single Kafka call.
	if len(validMessages) > 0 {
		if err := p.writer.WriteMessages(ctx, validMessages...); err != nil {
			// Batch write failed — route all valid events to DLQ.
			for _, idx := range validIndices {
				dlqID := p.writeToDLQ(ctx, DLQEvent{
					TopicName:    topic,
					KafkaOffset:  -1,
					Payload:      events[idx].Payload,
					ErrorType:    "publish_error",
					ErrorMessage: fmt.Sprintf("batch kafka write failed: %v", err),
					ProducerID:   events[idx].ProducerID,
				})
				results[idx] = PublishResult{
					Success:    false,
					Topic:      topic,
					DLQEventID: dlqID,
					Error:      err.Error(),
				}
			}
		} else {
			// Batch write succeeded.
			for _, idx := range validIndices {
				results[idx] = PublishResult{
					Success: true,
					Topic:   topic,
				}
			}
		}
	}

	return results
}

// Close shuts down the Kafka writer, flushing any buffered messages.
// Always call on server shutdown.
func (p *Producer) Close() error {
	return p.writer.Close()
}

// ── helpers ──────────────────────────────────────────────────────────────────

// BatchEvent is a single event in a PublishBatch call.
type BatchEvent struct {
	Key        string
	Payload    []byte
	ProducerID string
}

// writeToDLQ writes a failed event to the DLQ store.
// Returns the DLQ event ID on success, empty string on failure.
// Failures here are logged but not propagated — DLQ write failure
// should never mask the original publish failure.
func (p *Producer) writeToDLQ(ctx context.Context, event DLQEvent) string {
	// Generate an ID if the store doesn't assign one.
	if event.ProducerID == "" {
		event.ProducerID = uuid.New().String()
	}

	id, err := p.dlqStore.Insert(ctx, event)
	if err != nil {
		// DLQ write failed — log loudly but don't crash.
		// In production you'd alert on this: events are being lost.
		log.Printf("CRITICAL: failed to write event to DLQ for topic %s: %v",
			event.TopicName, err)
		return ""
	}

	return id
}

// formatValidationErrors joins multiple validation errors into a
// single string for storage in the DLQ error_message column.
func formatValidationErrors(errors []string) string {
	if len(errors) == 0 {
		return "validation failed (no details)"
	}
	result := ""
	for i, e := range errors {
		if i > 0 {
			result += "; "
		}
		result += e
	}
	return result
}
