-- Dead Letter Queue.
-- Every event that fails schema validation or processing lands here.
-- Full payload preserved so events can be inspected and replayed.
CREATE TABLE IF NOT EXISTS dead_letter_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    
    -- Where the event came from
    topic_name   VARCHAR(255) NOT NULL,
    partition_id INT NOT NULL DEFAULT 0,
    
    -- kafka_offset is the original offset in the Kafka topic.
    -- -1 means the event was rejected before reaching Kafka
    -- (schema validation failure at publish time).
    kafka_offset BIGINT NOT NULL DEFAULT -1,
    
    -- The original event payload — always preserved verbatim.
    payload      JSONB NOT NULL,
    
    -- Why it failed
    error_type   VARCHAR(100) NOT NULL, -- e.g. "schema_validation", "processing_error"
    error_message TEXT NOT NULL,
    
    -- Retry tracking
    retry_count  INT NOT NULL DEFAULT 0,
    status       VARCHAR(50) NOT NULL DEFAULT 'pending',
    -- status values: pending | retrying | retried | discarded
    
    -- Producer metadata for debugging
    producer_id  VARCHAR(255),
    
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_retry_at TIMESTAMPTZ
);

-- Most DLQ queries filter by topic + status.
CREATE INDEX IF NOT EXISTS idx_dlq_topic_status 
    ON dead_letter_events(topic_name, status);

-- Dashboard sorts by newest first.
CREATE INDEX IF NOT EXISTS idx_dlq_created_at 
    ON dead_letter_events(created_at DESC);