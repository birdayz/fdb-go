import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { EventService } from "@/gen/metrognome/v1/event_pb";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import { Zap, Send, BarChart3, Search, ChevronDown, Clock, Hash, Activity } from "lucide-react";

const eventClient = createClient(EventService, transport);
const customerClient = createClient(CustomerService, transport);
const meterClient = createClient(MeterService, transport);

export function EventsPage() {
  const [tab, setTab] = useState<"ingest" | "query">("ingest");
  const [customers, setCustomers] = useState<any[]>([]);
  const [meters, setMeters] = useState<any[]>([]);

  // Ingest state
  const [customerId, setCustomerId] = useState("");
  const [eventType, setEventType] = useState("");
  const [value, setValue] = useState("1");
  const [count, setCount] = useState("10");
  const [result, setResult] = useState<{ accepted: number; duplicates: number } | null>(null);
  const [ingesting, setIngesting] = useState(false);

  // Query state
  const [queryCustomerId, setQueryCustomerId] = useState("");
  const [queryMeterSlug, setQueryMeterSlug] = useState("");
  const [queryHours, setQueryHours] = useState("24");
  const [usageResult, setUsageResult] = useState<{ total: string; count: string } | null>(null);
  const [querying, setQuerying] = useState(false);

  useEffect(() => {
    customerClient.listCustomers({}).then(r => setCustomers(r.customers)).catch(() => {});
    meterClient.listMeters({}).then(r => setMeters(r.meters)).catch(() => {});
  }, []);

  async function handleIngest() {
    if (!customerId || !eventType) return;
    setIngesting(true);
    try {
      const n = parseInt(count) || 1;
      const v = parseInt(value) || 1;
      const events = Array.from({ length: n }, (_, i) => ({
        customerId,
        eventType,
        value: BigInt(v),
        timestampMs: BigInt(Date.now()),
        idempotencyKey: `ui-${Date.now()}-${Math.random().toString(36).slice(2)}-${i}`,
        propertiesJson: "",
      }));
      const resp = await eventClient.ingestEvents({ events });
      setResult({ accepted: resp.accepted, duplicates: resp.duplicates });
    } catch (e: any) {
      setResult({ accepted: 0, duplicates: 0 });
    }
    setIngesting(false);
  }

  async function handleQuery() {
    if (!queryCustomerId || !queryMeterSlug) return;
    setQuerying(true);
    try {
      const hours = parseInt(queryHours) || 24;
      const now = Date.now();
      const start = now - hours * 60 * 60 * 1000;
      const resp = await eventClient.getUsage({
        customerId: queryCustomerId,
        meterSlug: queryMeterSlug,
        startMs: BigInt(start),
        endMs: BigInt(now),
      });
      setUsageResult({ total: resp.totalValue.toString(), count: resp.eventCount?.toString() || "0" });
    } catch (e: any) {
      setUsageResult({ total: "error", count: "0" });
    }
    setQuerying(false);
  }

  return (
    <div className="p-8 max-w-5xl">
      <div className="mb-6">
        <h1 className="text-2xl font-bold text-gray-900">Events</h1>
        <p className="text-sm text-gray-500 mt-1">Ingest usage events and query aggregated usage data.</p>
      </div>

      {/* Tabs */}
      <div className="flex gap-1 bg-gray-100 rounded-lg p-1 mb-6 w-fit">
        <button onClick={() => setTab("ingest")}
          className={`flex items-center gap-2 px-4 py-2 rounded-md text-sm font-medium transition-colors ${tab === "ingest" ? "bg-white shadow-sm text-gray-900" : "text-gray-500 hover:text-gray-700"}`}>
          <Send className="w-4 h-4" /> Ingest Events
        </button>
        <button onClick={() => setTab("query")}
          className={`flex items-center gap-2 px-4 py-2 rounded-md text-sm font-medium transition-colors ${tab === "query" ? "bg-white shadow-sm text-gray-900" : "text-gray-500 hover:text-gray-700"}`}>
          <BarChart3 className="w-4 h-4" /> Query Usage
        </button>
      </div>

      {tab === "ingest" ? (
        <div className="bg-white rounded-xl border border-gray-200 p-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-1">Send Usage Events</h3>
          <p className="text-sm text-gray-500 mb-5">Simulate event ingestion. Each event gets a unique idempotency key.</p>

          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Customer</label>
              <select value={customerId} onChange={e => setCustomerId(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select customer...</option>
                {customers.map(c => <option key={c.id} value={c.id}>{c.name} ({c.id.slice(0,12)}…)</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Meter / Event Type</label>
              <select value={eventType} onChange={e => setEventType(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select meter...</option>
                {meters.map(m => <option key={m.slug} value={m.slug}>{m.name} ({m.slug})</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Value per event</label>
              <input type="number" value={value} onChange={e => setValue(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Number of events</label>
              <input type="number" value={count} onChange={e => setCount(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
          </div>

          <button onClick={handleIngest} disabled={ingesting || !customerId || !eventType}
            className="mt-5 flex items-center gap-2 px-5 py-2.5 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 disabled:opacity-50 disabled:cursor-not-allowed">
            <Zap className="w-4 h-4" /> {ingesting ? "Sending..." : "Send Events"}
          </button>

          {result && (
            <div className="mt-5 flex gap-4">
              <div className="flex-1 p-4 bg-emerald-50 rounded-lg border border-emerald-200">
                <div className="text-2xl font-bold text-emerald-700">{result.accepted}</div>
                <div className="text-xs text-emerald-600 mt-0.5">Accepted</div>
              </div>
              <div className="flex-1 p-4 bg-amber-50 rounded-lg border border-amber-200">
                <div className="text-2xl font-bold text-amber-700">{result.duplicates}</div>
                <div className="text-xs text-amber-600 mt-0.5">Duplicates</div>
              </div>
            </div>
          )}
        </div>
      ) : (
        <div className="bg-white rounded-xl border border-gray-200 p-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-1">Query Aggregated Usage</h3>
          <p className="text-sm text-gray-500 mb-5">Read SUM aggregates from atomic indexes — O(1) reads regardless of event count.</p>

          <div className="grid grid-cols-3 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Customer</label>
              <select value={queryCustomerId} onChange={e => setQueryCustomerId(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select customer...</option>
                {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Meter</label>
              <select value={queryMeterSlug} onChange={e => setQueryMeterSlug(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select meter...</option>
                {meters.map(m => <option key={m.slug} value={m.slug}>{m.name} ({m.slug})</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Time range</label>
              <select value={queryHours} onChange={e => setQueryHours(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="1">Last 1 hour</option>
                <option value="6">Last 6 hours</option>
                <option value="24">Last 24 hours</option>
                <option value="168">Last 7 days</option>
                <option value="720">Last 30 days</option>
              </select>
            </div>
          </div>

          <button onClick={handleQuery} disabled={querying || !queryCustomerId || !queryMeterSlug}
            className="mt-5 flex items-center gap-2 px-5 py-2.5 bg-gray-900 text-white rounded-lg text-sm font-medium hover:bg-gray-800 disabled:opacity-50 disabled:cursor-not-allowed">
            <Search className="w-4 h-4" /> {querying ? "Querying..." : "Query Usage"}
          </button>

          {usageResult && (
            <div className="mt-5 flex gap-4">
              <div className="flex-1 p-5 bg-indigo-50 rounded-lg border border-indigo-200">
                <div className="flex items-center gap-2 text-indigo-600 mb-1">
                  <Activity className="w-4 h-4" />
                  <span className="text-xs font-medium uppercase tracking-wider">Total Value</span>
                </div>
                <div className="text-3xl font-bold text-indigo-900">{usageResult.total}</div>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
