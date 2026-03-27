package dlq

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/djism/conduit/internal/db"
	"github.com/djism/conduit/internal/kafka"
	"github.com/google/uuid"
)

const (
	// StatusPending is the initial status when an event enters the DLQ.
	StatusPending = "pending"

	// StatusRetrying is set while a replay attempt is in progress.
	// Prevents concurrent retries of the same event.
	StatusRetrying = "retrying"

	// StatusRetried means the event was successfully replayed.
	// It went back into Kafka and was accepted.
	StatusRetried = "retried"

	// StatusDiscarded means the operator explicitly discarded the event
	// or the max retry count was exceeded.
	StatusDiscarded = "discarded"

	// MaxRetries is the maximum number of replay attempts before
	// an event is automatically moved to StatusDiscarded.
	// Prevents infinite retry loops on permanently broken events.
	MaxRetries = 5
)

// Event represents a single entry in the dead letter queue.
// Maps directly to the dead_letter_events table.
type Event struct {
	ID           string
	TopicName    string
	PartitionID  int
	KafkaOffset  int64
	Payload      []byte
	ErrorType    string
	ErrorMessage string
	RetryCount   int
	Status       string
	ProducerID   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastRetryAt  *time.Time // nil if never retried
}

// ListFilter controls which DLQ events are returned by List().
// All fields are optional — zero value means no filter applied.
type ListFilter struct {
	TopicName string // filter by topic
	Status    string // filter by status (pending, retried, discarded)
	Limit     int    // max results (default 50)
	Offset    int    // pagination offset
}

// Store manages DLQ events in PostgreSQL.
// It implements the kafka.DLQStore interface so the producer
// can write failed events without importing this package directly.
type Store struct {
	db *db.DB
}

// New creates a DLQ Store backed by PostgreSQL.
func New(database *db.DB) *Store {
	return &Store{db: database}
}

// Insert writes a failed event into the DLQ.
// Returns the generated UUID for the new DLQ entry.
//
// This implements the kafka.DLQStore interface —
// the producer calls this whenever an event fails
// schema validation or Kafka publish.
//
// Never returns a partial insert — if the INSERT fails,
// the error propagates and the producer logs it.
func (s *Store) Insert(ctx context.Context, event kafka.DLQEvent) (string, error) {
	id := uuid.New().String()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO dead_letter_events (
			id,
			topic_name,
			partition_id,
			kafka_offset,
			payload,
			error_type,
			error_message,
			producer_id,
			status,
			retry_count
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id,
		event.TopicName,
		event.PartitionID,
		event.KafkaOffset,
		event.Payload,
		event.ErrorType,
		event.ErrorMessage,
		event.ProducerID,
		StatusPending,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("inserting DLQ event for topic %s: %w",
			event.TopicName, err)
	}

	return id, nil
}

// GetByID fetches a single DLQ event by its UUID.
// Returns sql.ErrNoRows if not found — callers should check for this.
func (s *Store) GetByID(ctx context.Context, id string) (*Event, error) {
	var e Event
	var lastRetryAt sql.NullTime

	err := s.db.QueryRowContext(ctx,
		`SELECT
			id, topic_name, partition_id, kafka_offset,
			payload, error_type, error_message,
			retry_count, status, producer_id,
			created_at, updated_at, last_retry_at
		 FROM dead_letter_events
		 WHERE id = $1`,
		id,
	).Scan(
		&e.ID, &e.TopicName, &e.PartitionID, &e.KafkaOffset,
		&e.Payload, &e.ErrorType, &e.ErrorMessage,
		&e.RetryCount, &e.Status, &e.ProducerID,
		&e.CreatedAt, &e.UpdatedAt, &lastRetryAt,
	)
	if err != nil {
		return nil, fmt.Errorf("fetching DLQ event %s: %w", id, err)
	}

	if lastRetryAt.Valid {
		e.LastRetryAt = &lastRetryAt.Time
	}

	return &e, nil
}

// List returns DLQ events matching the given filter.
// Results are ordered newest-first — operators care most about
// recent failures when triaging incidents.
//
// Default limit is 50 if filter.Limit is zero.
// Supports pagination via filter.Offset.
func (s *Store) List(ctx context.Context, filter ListFilter) ([]*Event, error) {
	if filter.Limit == 0 {
		filter.Limit = 50
	}

	// Build query dynamically based on which filters are set.
	// Using positional parameters ($1, $2...) prevents SQL injection.
	query := `
		SELECT
			id, topic_name, partition_id, kafka_offset,
			payload, error_type, error_message,
			retry_count, status, producer_id,
			created_at, updated_at, last_retry_at
		FROM dead_letter_events
		WHERE 1=1`

	args := []interface{}{}
	argIdx := 1

	if filter.TopicName != "" {
		query += fmt.Sprintf(" AND topic_name = $%d", argIdx)
		args = append(args, filter.TopicName)
		argIdx++
	}

	if filter.Status != "" {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, filter.Status)
		argIdx++
	}

	query += fmt.Sprintf(
		" ORDER BY created_at DESC LIMIT $%d OFFSET $%d",
		argIdx, argIdx+1,
	)
	args = append(args, filter.Limit, filter.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing DLQ events: %w", err)
	}
	defer rows.Close()

	var events []*Event
	for rows.Next() {
		var e Event
		var lastRetryAt sql.NullTime

		if err := rows.Scan(
			&e.ID, &e.TopicName, &e.PartitionID, &e.KafkaOffset,
			&e.Payload, &e.ErrorType, &e.ErrorMessage,
			&e.RetryCount, &e.Status, &e.ProducerID,
			&e.CreatedAt, &e.UpdatedAt, &lastRetryAt,
		); err != nil {
			return nil, fmt.Errorf("scanning DLQ event row: %w", err)
		}

		if lastRetryAt.Valid {
			e.LastRetryAt = &lastRetryAt.Time
		}

		events = append(events, &e)
	}

	return events, nil
}

// MarkRetrying transitions an event to StatusRetrying.
// Called at the start of a replay attempt to prevent concurrent retries.
//
// Uses an optimistic lock — only transitions if current status is
// "pending". If the event is already being retried, returns an error
// rather than allowing two concurrent replay attempts on the same event.
func (s *Store) MarkRetrying(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE dead_letter_events
		 SET status = $1, updated_at = NOW()
		 WHERE id = $2 AND status = $3`,
		StatusRetrying, id, StatusPending,
	)
	if err != nil {
		return fmt.Errorf("marking event %s as retrying: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}

	// Zero rows affected means the event wasn't in "pending" status —
	// either already retrying (concurrent attempt) or already retried/discarded.
	if rows == 0 {
		return fmt.Errorf("event %s is not in pending status — cannot retry", id)
	}

	return nil
}

// MarkRetried transitions an event to StatusRetried after a
// successful replay. Increments retry_count and records the timestamp.
func (s *Store) MarkRetried(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dead_letter_events
		 SET
		 	status = $1,
		 	retry_count = retry_count + 1,
		 	last_retry_at = NOW(),
		 	updated_at = NOW()
		 WHERE id = $2`,
		StatusRetried, id,
	)
	if err != nil {
		return fmt.Errorf("marking event %s as retried: %w", id, err)
	}
	return nil
}

// MarkFailed transitions a retrying event back to pending after a
// failed replay attempt. Increments retry_count.
// If retry_count exceeds MaxRetries, moves to StatusDiscarded instead —
// prevents infinite retry loops on permanently broken events.
func (s *Store) MarkFailed(ctx context.Context, id string, errMsg string) error {
	// Fetch current retry count to check against MaxRetries.
	var retryCount int
	err := s.db.QueryRowContext(ctx,
		`SELECT retry_count FROM dead_letter_events WHERE id = $1`,
		id,
	).Scan(&retryCount)
	if err != nil {
		return fmt.Errorf("fetching retry count for event %s: %w", id, err)
	}

	newStatus := StatusPending
	if retryCount+1 >= MaxRetries {
		// Max retries exceeded — discard the event.
		// Operator can still manually discard/inspect it.
		newStatus = StatusDiscarded
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE dead_letter_events
		 SET
		 	status = $1,
		 	retry_count = retry_count + 1,
		 	error_message = $2,
		 	last_retry_at = NOW(),
		 	updated_at = NOW()
		 WHERE id = $3`,
		newStatus, errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("marking event %s as failed: %w", id, err)
	}

	return nil
}

// Discard permanently discards a DLQ event.
// Called when the operator explicitly decides not to retry an event —
// for example, a test event, a duplicate, or an event from a
// deprecated producer.
func (s *Store) Discard(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dead_letter_events
		 SET status = $1, updated_at = NOW()
		 WHERE id = $2`,
		StatusDiscarded, id,
	)
	if err != nil {
		return fmt.Errorf("discarding DLQ event %s: %w", id, err)
	}
	return nil
}

// Stats returns a summary count of DLQ events by status for a topic.
// Used by the dashboard summary cards.
// Returns map[status]count — e.g. {"pending": 12, "retried": 45, "discarded": 3}
func (s *Store) Stats(ctx context.Context, topicName string) (map[string]int64, error) {
	query := `
		SELECT status, COUNT(*) as count
		FROM dead_letter_events`

	args := []interface{}{}
	if topicName != "" {
		query += " WHERE topic_name = $1"
		args = append(args, topicName)
	}

	query += " GROUP BY status"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying DLQ stats: %w", err)
	}
	defer rows.Close()

	stats := map[string]int64{
		StatusPending:   0,
		StatusRetrying:  0,
		StatusRetried:   0,
		StatusDiscarded: 0,
	}

	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scanning stats row: %w", err)
		}
		stats[status] = count
	}

	return stats, nil
}