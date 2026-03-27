// ── Core Types ────────────────────────────────────────────────────────────────

export interface Topic {
    id: string;
    name: string;
    description?: string;
    partitions: number;
    replicationFactor: number;
    createdAt: string;
  }
  
  export interface Schema {
    id: string;
    topicName: string;
    version: number;
    schemaJson: Record<string, unknown>;
    isActive: boolean;
    createdAt: string;
  }
  
  // ── Dead Letter Queue ─────────────────────────────────────────────────────────
  
  export interface DLQEvent {
    id: string;
    topicName: string;
    partitionId: number;
    kafkaOffset: number;
    payload: Record<string, unknown>;
    errorType: string;
    errorMessage: string;
    retryCount: number;
    status: "pending" | "retrying" | "retried" | "discarded";
    producerId?: string;
    createdAt: string;
    updatedAt: string;
    lastRetryAt?: string;
  }
  
  export interface DLQStats {
    pending: number;
    retrying: number;
    retried: number;
    discarded: number;
    total: number;
  }
  
  export interface RetryResult {
    success: boolean;
    eventId: string;
    message: string;
  }
  
  // ── Lag Tracking ──────────────────────────────────────────────────────────────
  
  export interface PartitionLag {
    topic: string;
    partition: number;
    consumerGroup: string;
    latestOffset: number;
    committedOffset: number;
    lag: number;
  }
  
  export interface LagUpdate {
    topic: string;
    consumerGroup: string;
    totalLag: number;
    partitions: PartitionLag[];
    timestamp: string;
    isAlert: boolean;
  }
  
  export interface LagPoint {
    timestamp: string;
    lag: number;
  }
  
  // ── Pipeline Topology ─────────────────────────────────────────────────────────
  
  export type NodeType = "producer" | "topic" | "consumer_group";
  
  export interface TopologyNode {
    id: string;
    type: NodeType;
    label: string;
    metadata?: Record<string, unknown>;
  }
  
  export interface TopologyEdge {
    source: string;
    target: string;
    eventRate?: number;
  }
  
  export interface TopologyGraph {
    nodes: TopologyNode[];
    edges: TopologyEdge[];
  }
  
  // ── D3 force simulation needs x/y coordinates added at runtime ───────────────
  
  export interface D3Node extends TopologyNode {
    x?: number;
    y?: number;
    vx?: number;
    vy?: number;
    fx?: number | null;
    fy?: number | null;
  }
  
  export interface D3Edge {
    source: D3Node | string;
    target: D3Node | string;
    eventRate?: number;
  }