import { ApolloProvider } from "@apollo/client/react";
import client from "./lib/apollo";
import PipelineTopology from "./components/PipelineTopology";
import LagDashboard from "./components/LagDashboard";
import DeadLetterQueue from "./components/DeadLetterQueue";

const TOPICS = ["orders", "payments", "notifications"];
const CONSUMER_GROUP = "conduit-server";
const ALERT_THRESHOLD = 1000;

export default function App() {
  return (
    <ApolloProvider client={client}>
      <div className="min-h-screen bg-slate-950 text-slate-100">
        <header className="border-b border-slate-800 px-6 py-4">
          <div className="max-w-7xl mx-auto flex items-center justify-between">
            <div className="flex items-center gap-3">
              <div className="w-7 h-7 rounded-lg bg-indigo-500 flex items-center justify-center text-white font-bold text-sm">
                C
              </div>
              <span className="font-semibold text-lg tracking-tight">Conduit</span>
              <span className="text-xs text-slate-500 font-mono bg-slate-800 px-2 py-0.5 rounded">
                Event Pipeline Platform
              </span>
            </div>
            <div className="flex items-center gap-2 text-xs text-slate-500 font-mono">
              <span className="w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse inline-block" />
              live
            </div>
          </div>
        </header>
        <main className="max-w-7xl mx-auto px-6 py-6 space-y-6">
          <PipelineTopology width={1100} height={420} />
          <div className="space-y-4">
            <h3 className="text-slate-400 text-sm font-medium uppercase tracking-wider">
              Consumer Lag
            </h3>
            <div className="grid grid-cols-1 gap-4">
              {TOPICS.map((topic) => (
                <LagDashboard
                  key={topic}
                  topic={topic}
                  consumerGroup={CONSUMER_GROUP}
                  alertThreshold={ALERT_THRESHOLD}
                />
              ))}
            </div>
          </div>
          <DeadLetterQueue />
        </main>
      </div>
    </ApolloProvider>
  );
}
