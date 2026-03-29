package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/djism/conduit/internal/db"
	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Schema represents a registered JSON Schema for a topic.
// Each topic has one active schema at a time.
// Old versions are kept for audit and replay purposes —
// if you need to replay DLQ events from 3 weeks ago, you need
// the schema that was active when those events were published.
type Schema struct {
	ID         string
	TopicName  string
	Version    int
	SchemaJSON map[string]interface{}
	IsActive   bool
	CreatedAt  time.Time
}

// ValidationResult holds the outcome of validating an event payload.
// On success, IsValid is true and Errors is empty.
// On failure, IsValid is false and Errors contains human-readable
// descriptions of every constraint violation.
type ValidationResult struct {
	IsValid  bool
	Errors   []string
	SchemaID string // which schema version was used
}

// SchemaRegistry manages JSON Schema definitions for all topics.
// It is the single source of truth for what a valid event looks like.
//
// Every publish call goes through Validate() before Kafka sees the event.
// If validation fails, the event goes directly to the DLQ —
// it never enters the Kafka topic.
type SchemaRegistry struct {
	db *db.DB
}

// NewSchemaRegistry creates a SchemaRegistry backed by PostgreSQL.
func NewSchemaRegistry(database *db.DB) *SchemaRegistry {
	return &SchemaRegistry{db: database}
}

// Register stores a new JSON Schema for a topic.
//
// Versioning logic:
//   - First schema for a topic → version 1, active
//   - Subsequent schema → version N+1, active; old version deactivated
//
// This means schema evolution is automatic — you register a new schema
// and it immediately becomes the validator. Old schemas are preserved
// in the database for audit and DLQ replay.
//
// The schemaJSON parameter must be a valid JSON Schema document.
// We compile it with jsonschema to catch errors at registration time —
// better to fail here than silently accept an invalid schema that
// never matches anything.
func (r *SchemaRegistry) Register(
	ctx context.Context,
	topicName string,
	schemaJSON map[string]interface{},
) (*Schema, error) {
	// Validate that the schema itself is a valid JSON Schema document
	// before storing it. A malformed schema that passes registration
	// would silently fail to validate events later.
	if err := r.compileSchema(schemaJSON); err != nil {
		return nil, fmt.Errorf("invalid JSON Schema document: %w", err)
	}

	// Serialize to JSONB for PostgreSQL storage.
	schemaBytes, err := json.Marshal(schemaJSON)
	if err != nil {
		return nil, fmt.Errorf("serializing schema: %w", err)
	}

	// Use a transaction so deactivating the old schema and inserting
	// the new one are atomic. If either fails, neither happens.
	// Without a transaction, a crash between the two operations would
	// leave no active schema for the topic.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() // no-op if Commit() succeeds

	// Get current max version for this topic.
	// If no rows exist yet, version starts at 0 and we'll set it to 1.
	// Upsert the topic record — schemas table has a NOT NULL topic_id FK.
	// If the topic already exists this is a no-op.
	var topicID string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO topics (name, partitions, replication_factor)
		 VALUES ($1, 1, 1)
		 ON CONFLICT (name) DO UPDATE SET updated_at = NOW()
		 RETURNING id`,
		topicName,
	).Scan(&topicID)
	if err != nil {
		return nil, fmt.Errorf("upserting topic: %w", err)
	}

	// Get current max version for this topic.
	var currentVersion int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schemas WHERE topic_name = $1`,
		topicName,
	).Scan(&currentVersion)
	if err != nil {
		return nil, fmt.Errorf("querying current version: %w", err)
	}

	newVersion := currentVersion + 1

	// Deactivate all existing schemas for this topic.
	// Only one version should be active at a time.
	_, err = tx.ExecContext(ctx,
		`UPDATE schemas SET is_active = FALSE WHERE topic_name = $1`,
		topicName,
	)
	if err != nil {
		return nil, fmt.Errorf("deactivating old schemas: %w", err)
	}

	// Insert the new schema version.
	schemaID := uuid.New().String()
	var createdAt time.Time
	err = tx.QueryRowContext(ctx,
		`INSERT INTO schemas (id, topic_id, topic_name, version, schema_json, is_active)
		 VALUES ($1, $2, $3, $4, $5, TRUE)
		 RETURNING created_at`,
		schemaID, topicID, topicName, newVersion, string(schemaBytes),
	).Scan(&createdAt)
	if err != nil {
		return nil, fmt.Errorf("inserting schema: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing schema registration: %w", err)
	}

	return &Schema{
		ID:         schemaID,
		TopicName:  topicName,
		Version:    newVersion,
		SchemaJSON: schemaJSON,
		IsActive:   true,
		CreatedAt:  createdAt,
	}, nil
}

// Validate checks an event payload against the active schema for a topic.
//
// Returns a ValidationResult indicating success or failure.
// Never returns an error — validation failures are expected business
// events, not system errors. A system error (DB down, schema missing)
// returns an error instead.
//
// Fast path: if no schema is registered for this topic, validation
// passes with a warning. This allows topics to operate before a schema
// is registered — useful during development and onboarding.
func (r *SchemaRegistry) Validate(
	ctx context.Context,
	topicName string,
	payload []byte,
) (*ValidationResult, error) {
	// Fetch the active schema for this topic.
	schema, err := r.GetActiveSchema(ctx, topicName)
	if err != nil {
		return nil, fmt.Errorf("fetching active schema for %s: %w", topicName, err)
	}

	// No schema registered — pass with advisory.
	// In production you'd make this configurable: strict mode rejects
	// events with no schema, permissive mode allows them.
	if schema == nil {
		return &ValidationResult{
			IsValid: true,
			Errors:  []string{"no schema registered for topic — validation skipped"},
		}, nil
	}

	// Compile the stored schema into a jsonschema.Schema validator.
	// This is ~microseconds — fast enough for the hot path.
	compiler := jsonschema.NewCompiler()

	schemaBytes, err := json.Marshal(schema.SchemaJSON)
	if err != nil {
		return nil, fmt.Errorf("marshaling schema for compilation: %w", err)
	}

	// AddResource registers the schema document with a synthetic URL.
	// jsonschema requires a URL identifier even for inline schemas.
	resourceURL := fmt.Sprintf("conduit://schemas/%s/%d", topicName, schema.Version)
	if err := compiler.AddResource(resourceURL, strings.NewReader(string(schemaBytes))); err != nil {
		return nil, fmt.Errorf("adding schema resource: %w", err)
	}

	compiled, err := compiler.Compile(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("compiling schema: %w", err)
	}

	// Unmarshal the event payload into an interface{} for validation.
	var eventData interface{}
	if err := json.Unmarshal(payload, &eventData); err != nil {
		return &ValidationResult{
			IsValid:  false,
			Errors:   []string{fmt.Sprintf("payload is not valid JSON: %v", err)},
			SchemaID: schema.ID,
		}, nil
	}

	// Validate the event against the compiled schema.
	// jsonschema returns a *jsonschema.ValidationError on failure —
	// we extract all nested errors to give the producer a complete
	// picture of what's wrong rather than stopping at the first error.
	if err := compiled.Validate(eventData); err != nil {
		validationErr, ok := err.(*jsonschema.ValidationError)
		if !ok {
			return nil, fmt.Errorf("unexpected validation error type: %w", err)
		}

		errors := extractValidationErrors(validationErr)
		return &ValidationResult{
			IsValid:  false,
			Errors:   errors,
			SchemaID: schema.ID,
		}, nil
	}

	return &ValidationResult{
		IsValid:  true,
		SchemaID: schema.ID,
	}, nil
}

// GetActiveSchema fetches the currently active schema for a topic.
// Returns nil (not an error) if no schema has been registered yet.
func (r *SchemaRegistry) GetActiveSchema(
	ctx context.Context,
	topicName string,
) (*Schema, error) {
	var s Schema
	var schemaJSONBytes []byte

	err := r.db.QueryRowContext(ctx,
		`SELECT id, topic_name, version, schema_json, is_active, created_at
		 FROM schemas
		 WHERE topic_name = $1 AND is_active = TRUE
		 LIMIT 1`,
		topicName,
	).Scan(
		&s.ID,
		&s.TopicName,
		&s.Version,
		&schemaJSONBytes,
		&s.IsActive,
		&s.CreatedAt,
	)
	if err != nil {
		// sql.ErrNoRows means no schema registered — not an error.
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("querying active schema for %s: %w", topicName, err)
	}

	if err := json.Unmarshal(schemaJSONBytes, &s.SchemaJSON); err != nil {
		return nil, fmt.Errorf("deserializing schema JSON: %w", err)
	}

	return &s, nil
}

// GetSchemaHistory returns all schema versions for a topic,
// ordered from newest to oldest.
// Used by the Schema Registry UI to show version history.
func (r *SchemaRegistry) GetSchemaHistory(
	ctx context.Context,
	topicName string,
) ([]*Schema, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, topic_name, version, schema_json, is_active, created_at
		 FROM schemas
		 WHERE topic_name = $1
		 ORDER BY version DESC`,
		topicName,
	)
	if err != nil {
		return nil, fmt.Errorf("querying schema history for %s: %w", topicName, err)
	}
	defer rows.Close()

	var schemas []*Schema
	for rows.Next() {
		var s Schema
		var schemaJSONBytes []byte

		if err := rows.Scan(
			&s.ID,
			&s.TopicName,
			&s.Version,
			&schemaJSONBytes,
			&s.IsActive,
			&s.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning schema row: %w", err)
		}

		if err := json.Unmarshal(schemaJSONBytes, &s.SchemaJSON); err != nil {
			return nil, fmt.Errorf("deserializing schema JSON: %w", err)
		}

		schemas = append(schemas, &s)
	}

	return schemas, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// compileSchema attempts to compile a JSON Schema document.
// Used at registration time to catch invalid schemas immediately.
// Returns an error if the schema document itself is malformed.
func (r *SchemaRegistry) compileSchema(schemaJSON map[string]interface{}) error {
	schemaBytes, err := json.Marshal(schemaJSON)
	if err != nil {
		return fmt.Errorf("marshaling schema: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("conduit://validate", strings.NewReader(string(schemaBytes))); err != nil {
		return fmt.Errorf("adding schema resource: %w", err)
	}

	if _, err := compiler.Compile("conduit://validate"); err != nil {
		return fmt.Errorf("compiling schema: %w", err)
	}

	return nil
}

// extractValidationErrors flattens a nested ValidationError tree
// into a slice of human-readable strings.
//
// jsonschema returns errors as a tree because a single payload can
// violate multiple constraints at multiple nesting levels.
// We flatten to a list so the API response and DLQ error_message
// field are easy to read.
func extractValidationErrors(err *jsonschema.ValidationError) []string {
	var errors []string

	// The top-level error message.
	if err.Message != "" {
		errors = append(errors, fmt.Sprintf("%s: %s",
			err.InstanceLocation, err.Message))
	}

	// Recursively extract nested errors.
	for _, cause := range err.Causes {
		errors = append(errors, extractValidationErrors(cause)...)
	}

	if len(errors) == 0 {
		errors = append(errors, err.Error())
	}

	return errors
}
