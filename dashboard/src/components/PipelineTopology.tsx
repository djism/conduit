import { useEffect, useRef, useCallback } from "react";
import * as d3 from "d3";
import { gql } from "@apollo/client";
import { useQuery, useSubscription } from "@apollo/client/react";
import type { D3Node, D3Edge, TopologyGraph, LagUpdate } from "../types/conduit";

const TOPOLOGY_QUERY = gql`
  query GetTopology {
    topology {
      nodes {
        id
        type
        label
        metadata
      }
      edges {
        source
        target
        eventRate
      }
    }
  }
`;

const LAG_SUBSCRIPTION = gql`
  subscription OnLagUpdate {
    lagUpdates {
      topic
      consumerGroup
      totalLag
      isAlert
      timestamp
    }
  }
`;

const NODE_COLORS: Record<string, string> = {
  producer: "#6366f1",
  topic: "#f59e0b",
  consumer_group: "#10b981",
};

const NODE_RADIUS: Record<string, number> = {
  producer: 18,
  topic: 24,
  consumer_group: 18,
};

const ALERT_COLOR = "#ef4444";

interface Props {
  width?: number;
  height?: number;
}

export default function PipelineTopology({ width = 900, height = 500 }: Props) {
  const svgRef = useRef<SVGSVGElement>(null);
  const alertTopics = useRef<Set<string>>(new Set());

  const { data: topologyData } = useQuery<{ topology: TopologyGraph }>(
    TOPOLOGY_QUERY,
    { pollInterval: 30000 }
  );

  useSubscription<{ lagUpdates: LagUpdate }>(LAG_SUBSCRIPTION, {
    onData: ({ data }: { data: { data?: { lagUpdates?: LagUpdate } } }) => {
      const update = data.data?.lagUpdates;
      if (!update) return;

      if (update.isAlert) {
        alertTopics.current.add(update.topic);
      } else {
        alertTopics.current.delete(update.topic);
      }

      if (svgRef.current) {
        d3.select(svgRef.current)
          .selectAll<SVGCircleElement, D3Node>("circle")
          .attr("fill", (d) => getNodeColor(d, alertTopics.current));
      }
    },
  });

  const buildGraph = useCallback(
    (topology: TopologyGraph) => {
      const svg = d3.select(svgRef.current!);
      svg.selectAll("*").remove();

      const nodes: D3Node[] = topology.nodes.map((n) => ({ ...n }));
      const edges: D3Edge[] = topology.edges.map((e) => ({ ...e }));

      const simulation = d3
        .forceSimulation<D3Node>(nodes)
        .force(
          "link",
          d3
            .forceLink<D3Node, D3Edge>(edges)
            .id((d) => d.id)
            .distance(120)
            .strength(0.8)
        )
        .force("charge", d3.forceManyBody().strength(-400))
        .force("center", d3.forceCenter(width / 2, height / 2))
        .force(
          "collision",
          d3.forceCollide<D3Node>().radius((d) => NODE_RADIUS[d.type] + 10)
        );

      svg
        .append("defs")
        .append("marker")
        .attr("id", "arrowhead")
        .attr("viewBox", "0 -5 10 10")
        .attr("refX", 28)
        .attr("refY", 0)
        .attr("markerWidth", 6)
        .attr("markerHeight", 6)
        .attr("orient", "auto")
        .append("path")
        .attr("d", "M0,-5L10,0L0,5")
        .attr("fill", "#94a3b8");

      const link = svg
        .append("g")
        .selectAll("line")
        .data(edges)
        .join("line")
        .attr("stroke", "#94a3b8")
        .attr("stroke-width", 1.5)
        .attr("stroke-opacity", 0.6)
        .attr("marker-end", "url(#arrowhead)");

      const nodeGroup = svg
        .append("g")
        .selectAll("g")
        .data(nodes)
        .join("g")
        .attr("cursor", "pointer")
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        .call(d3.drag<SVGGElement, D3Node>() as any)
        .on("start.drag", (event: d3.D3DragEvent<SVGGElement, D3Node, D3Node>, d: D3Node) => {
          if (!event.active) simulation.alphaTarget(0.3).restart();
          d.fx = d.x;
          d.fy = d.y;
        })
        .on("drag.drag", (event: d3.D3DragEvent<SVGGElement, D3Node, D3Node>, d: D3Node) => {
          d.fx = event.x;
          d.fy = event.y;
        })
        .on("end.drag", (event: d3.D3DragEvent<SVGGElement, D3Node, D3Node>, d: D3Node) => {
          if (!event.active) simulation.alphaTarget(0);
          d.fx = null;
          d.fy = null;
        });

      nodeGroup
        .append("circle")
        .attr("r", (d) => NODE_RADIUS[d.type])
        .attr("fill", (d) => getNodeColor(d, alertTopics.current))
        .attr("stroke", "#1e293b")
        .attr("stroke-width", 2);

      nodeGroup
        .filter((d) => d.type === "topic")
        .append("circle")
        .attr("r", (d) => NODE_RADIUS[d.type])
        .attr("fill", "none")
        .attr("stroke", NODE_COLORS.topic)
        .attr("stroke-width", 2)
        .attr("opacity", 0.6)
        .attr("class", "pulse-ring");

      nodeGroup
        .append("text")
        .text((d) => d.label)
        .attr("text-anchor", "middle")
        .attr("dy", (d) => NODE_RADIUS[d.type] + 14)
        .attr("fill", "#cbd5e1")
        .attr("font-size", "11px")
        .attr("font-family", "monospace");

      nodeGroup
        .append("text")
        .text((d) => d.type.replace("_", " "))
        .attr("text-anchor", "middle")
        .attr("dy", (d) => -(NODE_RADIUS[d.type] + 6))
        .attr("fill", "#64748b")
        .attr("font-size", "9px")
        .attr("font-family", "monospace");

      simulation.on("tick", () => {
        link
          .attr("x1", (d) => (d.source as D3Node).x ?? 0)
          .attr("y1", (d) => (d.source as D3Node).y ?? 0)
          .attr("x2", (d) => (d.target as D3Node).x ?? 0)
          .attr("y2", (d) => (d.target as D3Node).y ?? 0);

        nodeGroup.attr(
          "transform",
          (d) => `translate(${d.x ?? 0}, ${d.y ?? 0})`
        );
      });

      simulation.alpha(1).restart();
    },
    [width, height]
  );

  useEffect(() => {
    if (!topologyData?.topology) return;
    buildGraph(topologyData.topology);
  }, [topologyData, buildGraph]);

  return (
    <div className="bg-slate-900 rounded-xl border border-slate-700 p-4">
      <div className="flex items-center justify-between mb-4">
        <h2 className="text-slate-100 font-semibold text-lg">
          Pipeline Topology
        </h2>
        <div className="flex gap-4 text-xs text-slate-400">
          {Object.entries(NODE_COLORS).map(([type, color]) => (
            <span key={type} className="flex items-center gap-1.5">
              <span
                className="w-2.5 h-2.5 rounded-full inline-block"
                style={{ backgroundColor: color }}
              />
              {type.replace("_", " ")}
            </span>
          ))}
          <span className="flex items-center gap-1.5">
            <span
              className="w-2.5 h-2.5 rounded-full inline-block"
              style={{ backgroundColor: ALERT_COLOR }}
            />
            alert
          </span>
        </div>
      </div>

      <svg
        ref={svgRef}
        width={width}
        height={height}
        className="w-full"
        style={{ background: "#0f172a", borderRadius: "8px" }}
      />

      <style>{`
        .pulse-ring {
          animation: pulse 2s ease-out infinite;
          transform-origin: center;
        }
        @keyframes pulse {
          0%   { r: 24px; opacity: 0.6; }
          100% { r: 40px; opacity: 0; }
        }
      `}</style>
    </div>
  );
}

function getNodeColor(node: D3Node, alertTopics: Set<string>): string {
  if (node.type === "topic" && alertTopics.has(node.id)) {
    return ALERT_COLOR;
  }
  return NODE_COLORS[node.type] ?? "#64748b";
}