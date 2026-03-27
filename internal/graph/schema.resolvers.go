package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/djism/conduit/internal/dlq"
	"github.com/djism/conduit/internal/lag"
	"github.com/google/uuid"
)

// ── Mutations ─────────────────────────────────────────────────────────────────

// RegisterSchema stores a new JSON Schema version for a topic.
// The new schema immediately becomes the active validator —
// all subsequent publishes to this topic are validated against it.
func (r *mutationResolver) RegisterSchema(
	ctx context.Context,
	topicName string,
	schemaJSON map[string]any,
) (*Schema, error) {
	schema, err := r.Schemas.Register(ctx, topicName, schemaJSON)
	if err != nil {
		return nil, fmt.Errorf("registering schema: %w", err)
	}

	return &Schema{
		ID:        schema.ID,
		TopicName: schema.TopicName,
		Version:   schema.Version,
		SchemaJSON: schema.SchemaJSON,
		IsActive:  schema.IsActive,
		CreatedAt: schema.CreatedAt,
	}, nil
}

// RetryEvent replays a single DLQ event back into its original topic.
//
// State machine:
//  1. Mark event as "retrying" (prevents concurrent retries)
//  2. Re-validate payload against current active schema
//  3. Re-publish to Kafka
//  4. On success: mark as "retried"
//  5. On failure: mark as "failed" (increments retry count)
func (r *mutationResolver) RetryEvent(
	ctx context.Context,
	id string,
) (*RetryResult, error) {
	// Fetch the DLQ event.
	event, err := r.DLQ.GetByID(ctx, id)
	if err != nil {
		return &RetryResult{
			Success: false,
			EventID: id,
			Message: fmt.Sprintf("event not found: %v", err),
		}, nil
	}

	// Mark as retrying — optimistic lock prevents concurrent retries.
	if err := r.DLQ.MarkRetrying(ctx, id); err != nil {
		return &RetryResult{
			Success: false,
			EventID: id,
			Message: fmt.Sprintf("cannot retry: %v", err),
		}, nil
	}

	// Re-publish through the producer — this re-validates AND re-publishes.
	result := r.Producer.Publish(
		ctx,
		event.TopicName,
		uuid.New().String(), // new key for retry
		event.Payload,
		"conduit-dlq-replay",
	)

	if !result.Success {
		// Mark failed — increments retry count, resets to pending
		// unless MaxRetries exceeded (then discarded).
		_ = r.DLQ.MarkFailed(ctx, id, result.Error)
		return &RetryResult{
			Success: false,
			EventID: id,
			Message: fmt.Sprintf("replay failed: %s", result.Error),
		}, nil
	}

	// Success — mark as retried.
	if err := r.DLQ.MarkRetried(ctx, id); err != nil {
		// Non-fatal — event was replayed successfully even if
		// we couldn't update the status.
		fmt.Printf("warn: failed to mark event %s as retried: %v\n", id, err)
	}

	return &RetryResult{
		Success: true,
		EventID: id,
		Message: "event replayed successfully",
	}, nil
}

// RetryBatch replays multiple DLQ events.
// Each event is retried independently — one failure doesn't stop others.
func (r *mutationResolver) RetryBatch(
	ctx context.Context,
	ids []string,
) ([]*RetryResult, error) {
	results := make([]*RetryResult, 0, len(ids))
	for _, id := range ids {
		result, err := r.RetryEvent(ctx, id)
		if err != nil {
			results = append(results, &RetryResult{
				Success: false,
				EventID: id,
				Message: err.Error(),
			})
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

// DiscardEvent permanently discards a single DLQ event.
func (r *mutationResolver) DiscardEvent(ctx context.Context, id string) (bool, error) {
	if err := r.DLQ.Discard(ctx, id); err != nil {
		return false, fmt.Errorf("discarding event: %w", err)
	}
	return true, nil
}

// DiscardBatch permanently discards multiple DLQ events.
func (r *mutationResolver) DiscardBatch(ctx context.Context, ids []string) (bool, error) {
	for _, id := range ids {
		if err := r.DLQ.Discard(ctx, id); err != nil {
			return false, fmt.Errorf("discarding event %s: %w", id, err)
		}
	}
	return true, nil
}

// ── Queries ───────────────────────────────────────────────────────────────────

// Topics returns all topics Conduit manages.
// Metadata comes from PostgreSQL; live offset data comes from Kafka Admin.
func (r *queryResolver) Topics(ctx context.Context) ([]*Topic, error) {
	infos, err := r.Admin.GetTopicMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching topic metadata: %w", err)
	}

	topics := make([]*Topic, 0, len(infos))
	for _, info := range infos {
		topics = append(topics, &Topic{
			ID:                info.Name,
			Name:              info.Name,
			Partitions:        info.Partitions,
			ReplicationFactor: info.ReplicationFactor,
			CreatedAt:         time.Now(), // placeholder
		})
	}
	return topics, nil
}

// Topic returns a single topic by name.
func (r *queryResolver) Topic(ctx context.Context, name string) (*Topic, error) {
	topics, err := r.Topics(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range topics {
		if t.Name == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("topic %q not found", name)
}

// ActiveSchema returns the currently active schema for a topic.
func (r *queryResolver) ActiveSchema(
	ctx context.Context,
	topicName string,
) (*Schema, error) {
	s, err := r.Schemas.GetActiveSchema(ctx, topicName)
	if err != nil {
		return nil, fmt.Errorf("fetching active schema: %w", err)
	}
	if s == nil {
		return nil, nil
	}
	return &Schema{
		ID:         s.ID,
		TopicName:  s.TopicName,
		Version:    s.Version,
		SchemaJSON: s.SchemaJSON,
		IsActive:   s.IsActive,
		CreatedAt:  s.CreatedAt,
	}, nil
}

// SchemaHistory returns all schema versions for a topic.
func (r *queryResolver) SchemaHistory(
	ctx context.Context,
	topicName string,
) ([]*Schema, error) {
	schemas, err := r.Schemas.GetSchemaHistory(ctx, topicName)
	if err != nil {
		return nil, fmt.Errorf("fetching schema history: %w", err)
	}

	result := make([]*Schema, 0, len(schemas))
	for _, s := range schemas {
		result = append(result, &Schema{
			ID:         s.ID,
			TopicName:  s.TopicName,
			Version:    s.Version,
			SchemaJSON: s.SchemaJSON,
			IsActive:   s.IsActive,
			CreatedAt:  s.CreatedAt,
		})
	}
	return result, nil
}

// DlqEvents returns DLQ events matching the given filters.
func (r *queryResolver) DlqEvents(
	ctx context.Context,
	topicName *string,
	status *string,
	limit *int,
	offset *int,
) ([]*DLQEvent, error) {
	filter := dlq.ListFilter{}

	if topicName != nil {
		filter.TopicName = *topicName
	}
	if status != nil {
		filter.Status = *status
	}
	if limit != nil {
		filter.Limit = *limit
	}
	if offset != nil {
		filter.Offset = *offset
	}

	events, err := r.DLQ.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing DLQ events: %w", err)
	}

	result := make([]*DLQEvent, 0, len(events))
	for _, e := range events {
		// Deserialize payload bytes back to map for GraphQL Map scalar.
		var payloadMap map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payloadMap); err != nil {
			payloadMap = map[string]interface{}{"raw": string(e.Payload)}
		}

		gqlEvent := &DLQEvent{
			ID:           e.ID,
			TopicName:    e.TopicName,
			PartitionID:  e.PartitionID,
			KafkaOffset:  int(e.KafkaOffset),
			Payload:      payloadMap,
			ErrorType:    e.ErrorType,
			ErrorMessage: e.ErrorMessage,
			RetryCount:   e.RetryCount,
			Status:       e.Status,
			CreatedAt:    e.CreatedAt,
			UpdatedAt:    e.UpdatedAt,
		}

		if e.ProducerID != "" {
			gqlEvent.ProducerID = &e.ProducerID
		}
		if e.LastRetryAt != nil {
			gqlEvent.LastRetryAt = e.LastRetryAt
		}

		result = append(result, gqlEvent)
	}

	return result, nil
}

// DlqEvent returns a single DLQ event by ID.
func (r *queryResolver) DlqEvent(ctx context.Context, id string) (*DLQEvent, error) {
	events, err := r.DlqEvents(ctx, nil, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, fmt.Errorf("DLQ event %q not found", id)
}

// DlqStats returns counts per status for the DLQ summary cards.
func (r *queryResolver) DlqStats(
	ctx context.Context,
	topicName *string,
) (*DLQStats, error) {
	topic := ""
	if topicName != nil {
		topic = *topicName
	}

	stats, err := r.DLQ.Stats(ctx, topic)
	if err != nil {
		return nil, fmt.Errorf("fetching DLQ stats: %w", err)
	}

	pending := int(stats["pending"])
	retrying := int(stats["retrying"])
	retried := int(stats["retried"])
	discarded := int(stats["discarded"])

	return &DLQStats{
		Pending:   pending,
		Retrying:  retrying,
		Retried:   retried,
		Discarded: discarded,
		Total:     pending + retrying + retried + discarded,
	}, nil
}

// LagHistory returns historical lag readings for the time-series chart.
// durationMinutes controls how far back to fetch.
func (r *queryResolver) LagHistory(
	ctx context.Context,
	topic string,
	consumerGroup string,
	durationMinutes int,
) ([]*LagPoint, error) {
	duration := time.Duration(durationMinutes) * time.Minute
	points, err := r.Tracker.GetHistory(ctx, topic, consumerGroup, duration)
	if err != nil {
		return nil, fmt.Errorf("fetching lag history: %w", err)
	}

	result := make([]*LagPoint, 0, len(points))
	for _, p := range points {
		result = append(result, &LagPoint{
			Timestamp: p.Timestamp,
			Lag:       int(p.Lag),
		})
	}
	return result, nil
}

// LatestLag returns the most recent lag value for summary cards.
func (r *queryResolver) LatestLag(
	ctx context.Context,
	topic string,
	consumerGroup string,
) (int, error) {
	lag, err := r.Tracker.GetHistory(ctx, topic, consumerGroup, 1*time.Minute)
	if err != nil {
		return 0, fmt.Errorf("fetching latest lag: %w", err)
	}
	if len(lag) == 0 {
		return 0, nil
	}
	return int(lag[len(lag)-1].Lag), nil
}

// Topology builds the pipeline graph for the D3 force visualizer.
// Nodes: producers (simulated), topics, consumer groups.
// Edges: producer→topic, topic→consumerGroup.
func (r *queryResolver) Topology(ctx context.Context) (*TopologyGraph, error) {
	nodes := []*TopologyNode{}
	edges := []*TopologyEdge{}

	// Add a simulated producer node per topic.
	for _, topic := range r.Config.KafkaTopics {
		producerID := fmt.Sprintf("producer-%s", topic)
		nodes = append(nodes, &TopologyNode{
			ID:    producerID,
			Type:  "producer",
			Label: fmt.Sprintf("%s-producer", topic),
		})

		// Add topic node.
		nodes = append(nodes, &TopologyNode{
			ID:    topic,
			Type:  "topic",
			Label: topic,
		})

		// Producer → Topic edge.
		edges = append(edges, &TopologyEdge{
			Source: producerID,
			Target: topic,
		})

		// Consumer group node.
		cgID := fmt.Sprintf("cg-%s", r.Config.KafkaGroupID)
		nodes = append(nodes, &TopologyNode{
			ID:    cgID,
			Type:  "consumer_group",
			Label: r.Config.KafkaGroupID,
		})

		// Topic → Consumer Group edge.
		edges = append(edges, &TopologyEdge{
			Source: topic,
			Target: cgID,
		})
	}

	return &TopologyGraph{
		Nodes: nodes,
		Edges: edges,
	}, nil
}

// Health verifies all dependencies are reachable.
func (r *queryResolver) Health(ctx context.Context) (bool, error) {
	if err := r.Admin.HealthCheck(ctx); err != nil {
		return false, fmt.Errorf("kafka unhealthy: %w", err)
	}
	return true, nil
}

// ── Subscription ─────────────────────────────────────────────────────────────

// LagUpdates streams real-time lag metrics to the dashboard.
//
// How it works:
//  1. Register as a subscriber on the lag tracker
//  2. Create an output channel of *LagUpdate
//  3. Start a goroutine that reads from the tracker channel
//     and converts lag.Update → *LagUpdate for GraphQL
//  4. When the client disconnects (ctx.Done()), unsubscribe
//     and close the output channel
//
// The GraphQL engine reads from the returned channel and pushes
// each value to the connected WebSocket client automatically.
func (r *subscriptionResolver) LagUpdates(ctx context.Context) (<-chan *LagUpdate, error) {
	subscriberID := uuid.New().String()

	// Subscribe to the lag tracker.
	updateCh := r.Tracker.Subscribe(subscriberID)

	// Output channel for GraphQL — strongly typed *LagUpdate.
	out := make(chan *LagUpdate, 10)

	go func() {
		defer close(out)
		defer r.Tracker.Unsubscribe(subscriberID)

		for {
			select {
			case update, ok := <-updateCh:
				if !ok {
					// Tracker stopped — close the subscription.
					return
				}

				// Convert lag.Update → GraphQL *LagUpdate.
				partitions := make([]*PartitionLag, 0, len(update.Partitions))
				for _, p := range update.Partitions {
					partitions = append(partitions, &PartitionLag{
						Topic:           p.Topic,
						Partition:       p.Partition,
						ConsumerGroup:   p.ConsumerGroup,
						LatestOffset:    int(p.LatestOffset),
						CommittedOffset: int(p.CommittedOffset),
						Lag:             int(p.Lag),
					})
				}

				gqlUpdate := &LagUpdate{
					Topic:         update.Topic,
					ConsumerGroup: update.ConsumerGroup,
					TotalLag:      int(update.TotalLag),
					Partitions:    partitions,
					Timestamp:     update.Timestamp,
					IsAlert:       update.IsAlert,
				}

				// Non-blocking send — if the client can't keep up,
				// drop the update rather than blocking the goroutine.
				select {
				case out <- gqlUpdate:
				default:
				}

			case <-ctx.Done():
				// Client disconnected — clean up and exit.
				return
			}
		}
	}()

	return out, nil
}

// ── Internal resolver wiring ──────────────────────────────────────────────────

// Mutation returns MutationResolver implementation.
func (r *Resolver) Mutation() MutationResolver { return &mutationResolver{r} }

// Query returns QueryResolver implementation.
func (r *Resolver) Query() QueryResolver { return &queryResolver{r} }

// Subscription returns SubscriptionResolver implementation.
func (r *Resolver) Subscription() SubscriptionResolver { return &subscriptionResolver{r} }

type mutationResolver struct{ *Resolver }
type queryResolver struct{ *Resolver }
type subscriptionResolver struct{ *Resolver }

// lagUpdateFromTracker satisfies the compiler — unused but referenced.
var _ = lag.Update{}