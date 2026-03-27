-- Schema Registry.
-- Each topic can have multiple schema versions.
-- Only one version is active at a time — the latest one.
-- We keep old versions for audit and replay purposes.
CREATE TABLE IF NOT EXISTS schemas (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic_id   UUID NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    topic_name VARCHAR(255) NOT NULL,
    
    -- version is monotonically increasing per topic.
    -- New schema for same topic = version + 1.
    version    INT NOT NULL DEFAULT 1,
    
    -- schema_json is the raw JSON Schema document.
    -- Validated against the JSON Schema spec on insert.
    schema_json JSONB NOT NULL,
    
    -- is_active: only the active schema validates incoming events.
    -- When a new version is registered, the old one is deactivated.
    is_active  BOOLEAN NOT NULL DEFAULT TRUE,
    
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    -- A topic can only have one active schema at a time.
    UNIQUE(topic_name, version)
);

CREATE INDEX IF NOT EXISTS idx_schemas_topic_name ON schemas(topic_name);
CREATE INDEX IF NOT EXISTS idx_schemas_active ON schemas(topic_name, is_active) 
    WHERE is_active = TRUE;