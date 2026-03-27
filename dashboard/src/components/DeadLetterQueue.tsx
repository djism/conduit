import { useState } from "react";
import { gql } from "@apollo/client";
import { useQuery, useMutation } from "@apollo/client/react";
import {
  RefreshCw,
  Trash2,
  ChevronDown,
  ChevronRight,
  AlertCircle,
  CheckCircle,
  Clock,
  XCircle,
} from "lucide-react";
import type { DLQEvent, DLQStats } from "../types/conduit";

// ── GraphQL ───────────────────────────────────────────────────────────────────

const DLQ_EVENTS_QUERY = gql`
  query GetDLQEvents($topicName: String, $status: String, $limit: Int, $offset: Int) {
    dlqEvents(topicName: $topicName, status: $status, limit: $limit, offset: $offset) {
      id
      topicName
      partitionId
      kafkaOffset
      payload
      errorType
      errorMessage
      retryCount
      status
      producerId
      createdAt
      updatedAt
      lastRetryAt
    }
    dlqStats {
      pending
      retrying
      retried
      discarded
      total
    }
  }
`;

const RETRY_EVENT = gql`
  mutation RetryEvent($id: ID!) {
    retryEvent(id: $id) {
      success
      eventId
      message
    }
  }
`;

const RETRY_BATCH = gql`
  mutation RetryBatch($ids: [ID!]!) {
    retryBatch(ids: $ids) {
      success
      eventId
      message
    }
  }
`;

const DISCARD_EVENT = gql`
  mutation DiscardEvent($id: ID!) {
    discardEvent(id: $id)
  }
`;

const DISCARD_BATCH = gql`
  mutation DiscardBatch($ids: [ID!]!) {
    discardBatch(ids: $ids)
  }
`;

// ── Status config ─────────────────────────────────────────────────────────────

const STATUS_CONFIG = {
  pending: {
    label: "Pending",
    color: "text-amber-400",
    bg: "bg-amber-400/10",
    icon: <Clock size={12} />,
  },
  retrying: {
    label: "Retrying",
    color: "text-blue-400",
    bg: "bg-blue-400/10",
    icon: <RefreshCw size={12} className="animate-spin" />,
  },
  retried: {
    label: "Retried",
    color: "text-emerald-400",
    bg: "bg-emerald-400/10",
    icon: <CheckCircle size={12} />,
  },
  discarded: {
    label: "Discarded",
    color: "text-slate-500",
    bg: "bg-slate-500/10",
    icon: <XCircle size={12} />,
  },
};

// ── Component ─────────────────────────────────────────────────────────────────

export default function DeadLetterQueue() {
  const [statusFilter, setStatusFilter] = useState<string>("pending");
  const [topicFilter, setTopicFilter] = useState<string>("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [page, setPage] = useState(0);
  const PAGE_SIZE = 20;

  // ── Data fetching ─────────────────────────────────────────────────────────

  const { data, loading, refetch } = useQuery<{
    dlqEvents: DLQEvent[];
    dlqStats: DLQStats;
  }>(DLQ_EVENTS_QUERY, {
    variables: {
      topicName: topicFilter || null,
      status: statusFilter || null,
      limit: PAGE_SIZE,
      offset: page * PAGE_SIZE,
    },
    pollInterval: 10000, // refresh every 10s
  });

  // ── Mutations ─────────────────────────────────────────────────────────────

  const [retryEvent, { loading: retrying }] = useMutation(RETRY_EVENT, {
    onCompleted: () => refetch(),
  });

  const [retryBatch, { loading: retryingBatch }] = useMutation(RETRY_BATCH, {
    onCompleted: () => {
      setSelectedIds(new Set());
      refetch();
    },
  });

  const [discardEvent] = useMutation(DISCARD_EVENT, {
    onCompleted: () => refetch(),
  });

  const [discardBatch] = useMutation(DISCARD_BATCH, {
    onCompleted: () => {
      setSelectedIds(new Set());
      refetch();
    },
  });

  // ── Handlers ──────────────────────────────────────────────────────────────

  function toggleSelect(id: string) {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      next.has(id) ? next.delete(id) : next.add(id);
      return next;
    });
  }

  function toggleSelectAll() {
    if (!data?.dlqEvents) return;
    if (selectedIds.size === data.dlqEvents.length) {
      setSelectedIds(new Set());
    } else {
      setSelectedIds(new Set(data.dlqEvents.map((e) => e.id)));
    }
  }

  function handleRetrySelected() {
    retryBatch({ variables: { ids: Array.from(selectedIds) } });
  }

  function handleDiscardSelected() {
    discardBatch({ variables: { ids: Array.from(selectedIds) } });
  }

  // ── Render ────────────────────────────────────────────────────────────────

  const events = data?.dlqEvents ?? [];
  const stats = data?.dlqStats;

  return (
    <div className="bg-slate-900 rounded-xl border border-slate-700 p-4">

      {/* Header */}
      <div className="flex items-center justify-between mb-4">
        <div className="flex items-center gap-2">
          <AlertCircle size={18} className="text-red-400" />
          <h2 className="text-slate-100 font-semibold text-lg">
            Dead Letter Queue
          </h2>
        </div>
        <button
          onClick={() => refetch()}
          className="text-slate-400 hover:text-slate-200 transition-colors"
        >
          <RefreshCw size={16} />
        </button>
      </div>

      {/* Stats row */}
      {stats && (
        <div className="grid grid-cols-4 gap-2 mb-4">
          {Object.entries(STATUS_CONFIG).map(([status, config]) => (
            <button
              key={status}
              onClick={() => {
                setStatusFilter(status === statusFilter ? "" : status);
                setPage(0);
              }}
              className={`rounded-lg p-2.5 border transition-all text-left ${
                statusFilter === status
                  ? `${config.bg} border-current ${config.color}`
                  : "bg-slate-800 border-slate-700 text-slate-400 hover:border-slate-600"
              }`}
            >
              <div className="flex items-center gap-1.5 text-xs mb-1">
                {config.icon}
                {config.label}
              </div>
              <div className="text-lg font-mono font-bold">
                {stats[status as keyof DLQStats]}
              </div>
            </button>
          ))}
        </div>
      )}

      {/* Filters + bulk actions */}
      <div className="flex items-center gap-3 mb-3">
        <input
          type="text"
          placeholder="Filter by topic..."
          value={topicFilter}
          onChange={(e) => {
            setTopicFilter(e.target.value);
            setPage(0);
          }}
          className="flex-1 bg-slate-800 border border-slate-700 rounded-lg px-3 py-1.5
                     text-sm text-slate-300 placeholder-slate-500 focus:outline-none
                     focus:border-indigo-500 font-mono"
        />

        {selectedIds.size > 0 && (
          <div className="flex items-center gap-2">
            <span className="text-xs text-slate-400">
              {selectedIds.size} selected
            </span>
            <button
              onClick={handleRetrySelected}
              disabled={retryingBatch}
              className="flex items-center gap-1.5 px-3 py-1.5 bg-indigo-600
                         hover:bg-indigo-500 disabled:opacity-50 rounded-lg
                         text-xs text-white transition-colors"
            >
              <RefreshCw size={12} />
              Retry all
            </button>
            <button
              onClick={handleDiscardSelected}
              className="flex items-center gap-1.5 px-3 py-1.5 bg-slate-700
                         hover:bg-slate-600 rounded-lg text-xs text-slate-300
                         transition-colors"
            >
              <Trash2 size={12} />
              Discard all
            </button>
          </div>
        )}
      </div>

      {/* Table */}
      <div className="overflow-hidden rounded-lg border border-slate-800">
        <table className="w-full text-sm">
          <thead>
            <tr className="bg-slate-800/60 border-b border-slate-700">
              <th className="w-8 px-3 py-2">
                <input
                  type="checkbox"
                  checked={
                    events.length > 0 && selectedIds.size === events.length
                  }
                  onChange={toggleSelectAll}
                  className="accent-indigo-500"
                />
              </th>
              <th className="px-3 py-2 text-left text-xs font-medium text-slate-400">
                Topic
              </th>
              <th className="px-3 py-2 text-left text-xs font-medium text-slate-400">
                Error Type
              </th>
              <th className="px-3 py-2 text-left text-xs font-medium text-slate-400">
                Status
              </th>
              <th className="px-3 py-2 text-left text-xs font-medium text-slate-400">
                Retries
              </th>
              <th className="px-3 py-2 text-left text-xs font-medium text-slate-400">
                Time
              </th>
              <th className="px-3 py-2 text-left text-xs font-medium text-slate-400">
                Actions
              </th>
            </tr>
          </thead>
          <tbody>
            {loading && (
              <tr>
                <td colSpan={7} className="px-3 py-8 text-center text-slate-500">
                  Loading...
                </td>
              </tr>
            )}

            {!loading && events.length === 0 && (
              <tr>
                <td colSpan={7} className="px-3 py-8 text-center text-slate-500">
                  No events found
                </td>
              </tr>
            )}

            {events.map((event) => (
              <>
                <tr
                  key={event.id}
                  className={`border-b border-slate-800 hover:bg-slate-800/40 transition-colors
                    ${selectedIds.has(event.id) ? "bg-indigo-500/5" : ""}`}
                >
                  <td className="px-3 py-2.5">
                    <input
                      type="checkbox"
                      checked={selectedIds.has(event.id)}
                      onChange={() => toggleSelect(event.id)}
                      className="accent-indigo-500"
                    />
                  </td>

                  <td className="px-3 py-2.5 font-mono text-xs text-slate-300">
                    {event.topicName}
                  </td>

                  <td className="px-3 py-2.5">
                    <span className="font-mono text-xs text-orange-400 bg-orange-400/10
                                     px-1.5 py-0.5 rounded">
                      {event.errorType}
                    </span>
                  </td>

                  <td className="px-3 py-2.5">
                    <StatusBadge status={event.status} />
                  </td>

                  <td className="px-3 py-2.5 font-mono text-xs text-slate-400">
                    {event.retryCount}
                  </td>

                  <td className="px-3 py-2.5 text-xs text-slate-500 font-mono">
                    {new Date(event.createdAt).toLocaleTimeString()}
                  </td>

                  <td className="px-3 py-2.5">
                    <div className="flex items-center gap-2">
                      {/* Expand payload */}
                      <button
                        onClick={() =>
                          setExpandedId(
                            expandedId === event.id ? null : event.id
                          )
                        }
                        className="text-slate-400 hover:text-slate-200 transition-colors"
                        title="Inspect payload"
                      >
                        {expandedId === event.id ? (
                          <ChevronDown size={14} />
                        ) : (
                          <ChevronRight size={14} />
                        )}
                      </button>

                      {/* Retry */}
                      {event.status === "pending" && (
                        <button
                          onClick={() =>
                            retryEvent({ variables: { id: event.id } })
                          }
                          disabled={retrying}
                          className="text-indigo-400 hover:text-indigo-300
                                     disabled:opacity-40 transition-colors"
                          title="Retry event"
                        >
                          <RefreshCw size={14} />
                        </button>
                      )}

                      {/* Discard */}
                      {event.status === "pending" && (
                        <button
                          onClick={() =>
                            discardEvent({ variables: { id: event.id } })
                          }
                          className="text-slate-500 hover:text-red-400 transition-colors"
                          title="Discard event"
                        >
                          <Trash2 size={14} />
                        </button>
                      )}
                    </div>
                  </td>
                </tr>

                {/* Expanded payload row */}
                {expandedId === event.id && (
                  <tr
                    key={`${event.id}-expanded`}
                    className="bg-slate-950 border-b border-slate-800"
                  >
                    <td colSpan={7} className="px-4 py-3">
                      <div className="grid grid-cols-2 gap-4">
                        {/* Payload */}
                        <div>
                          <div className="text-xs text-slate-500 mb-1.5 font-medium">
                            PAYLOAD
                          </div>
                          <pre className="text-xs font-mono text-slate-300
                                          bg-slate-900 rounded p-3 overflow-auto
                                          max-h-48 border border-slate-800">
                            {JSON.stringify(event.payload, null, 2)}
                          </pre>
                        </div>

                        {/* Error details */}
                        <div>
                          <div className="text-xs text-slate-500 mb-1.5 font-medium">
                            ERROR
                          </div>
                          <div className="text-xs font-mono text-red-400
                                          bg-slate-900 rounded p-3 border
                                          border-slate-800 max-h-48 overflow-auto">
                            {event.errorMessage}
                          </div>

                          {/* Metadata */}
                          <div className="mt-3 space-y-1 text-xs font-mono text-slate-500">
                            <div>partition: {event.partitionId}</div>
                            <div>offset: {event.kafkaOffset}</div>
                            {event.producerId && (
                              <div>producer: {event.producerId}</div>
                            )}
                            {event.lastRetryAt && (
                              <div>
                                last retry:{" "}
                                {new Date(event.lastRetryAt).toLocaleString()}
                              </div>
                            )}
                          </div>
                        </div>
                      </div>
                    </td>
                  </tr>
                )}
              </>
            ))}
          </tbody>
        </table>
      </div>

      {/* Pagination */}
      <div className="flex items-center justify-between mt-3">
        <span className="text-xs text-slate-500">
          {events.length} events shown
        </span>
        <div className="flex gap-2">
          <button
            onClick={() => setPage((p) => Math.max(0, p - 1))}
            disabled={page === 0}
            className="px-3 py-1 text-xs bg-slate-800 hover:bg-slate-700
                       disabled:opacity-40 rounded text-slate-300 transition-colors"
          >
            Previous
          </button>
          <button
            onClick={() => setPage((p) => p + 1)}
            disabled={events.length < PAGE_SIZE}
            className="px-3 py-1 text-xs bg-slate-800 hover:bg-slate-700
                       disabled:opacity-40 rounded text-slate-300 transition-colors"
          >
            Next
          </button>
        </div>
      </div>
    </div>
  );
}

// ── Sub-components ────────────────────────────────────────────────────────────

function StatusBadge({ status }: { status: string }) {
  const config =
    STATUS_CONFIG[status as keyof typeof STATUS_CONFIG] ??
    STATUS_CONFIG.pending;

  return (
    <span
      className={`inline-flex items-center gap-1 text-xs font-medium px-1.5
                  py-0.5 rounded ${config.color} ${config.bg}`}
    >
      {config.icon}
      {config.label}
    </span>
  );
}