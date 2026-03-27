-- Topics Conduit manages.
-- Each topic has a name, optional description, and config metadata.
-- This is the parent table — schemas and DLQ events reference it.
CREATE TABLE IF NOT EXISTS topics (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        VARCHAR(255) NOT NULL UNIQUE,
    description TEXT,
    
    -- partitions and replication_factor mirror what's in Kafka.
    -- Stored here so the dashboard can show them without an Admin API call.
    partitions         INT NOT NULL DEFAULT 1,
    replication_factor INT NOT NULL DEFAULT 1,
    
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index on name because every lookup is by topic name.
CREATE INDEX IF NOT EXISTS idx_topics_name ON topics(name);