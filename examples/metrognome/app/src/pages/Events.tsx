import { useState, useEffect, useCallback } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { EventService } from "@/gen/metrognome/v1/event_pb";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import type { EventRecord } from "@/gen/metrognome/v1/event_pb";
import { Zap, Send, BarChart3, Search, Clock, Activity, List, Loader2, ChevronDown } from "lucide-react";

const eventClient = createClient(EventService, transport);
const customerClient = createClient(CustomerService, transport);
const meterClient = createClient(MeterService, transport);

function timeAgo(ms: bigint): string {
  const seconds = Math.floor((Date.now() - Number(ms)) / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

function formatTs(ms: bigint): string {
  return new Date(Number(ms)).toLocaleString();
}

// --- Browse Tab ---
function BrowseTab({ customers, meters }: { customers: any[]; meters: any[] }) {
  const [events, setEvents] = useState<EventRecord[]>([]);
  const [continuation, setContinuation] = useState<Uint8Array | null>(null);
  const [loading, setLoading] = useState(false);
  const [initialLoad, setInitialLoad] = useState(true);
  const [customerId, setCustomerId] = useState("");
  const [eventType, setEventType] = useState("");

  const PAGE_SIZE = 50;

  const loadPage = useCallback(async (reset: boolean) => {
    setLoading(true);
    try {
      const resp = await eventClient.listEvents({
        pageSize: PAGE_SIZE,
        continuationToken: reset ? new Uint8Array() : (continuation ?? new Uint8Array()),
        customerId,
        eventType,
      });
      if (reset) {
        setEvents(resp.events);
      } else {
        setEvents(prev => [...prev, ...resp.events]);
      }
      setContinuation(resp.continuationToken.length > 0 ? resp.continuationToken : null);
    } catch (e) {
      console.error("ListEvents failed:", e);
    }
    setLoading(false);
    setInitialLoad(false);
  }, [continuation, customerId, eventType]);

  // Initial load + reload on filter change
  useEffect(() => {
    setInitialLoad(true);
    setEvents([]);
    setContinuation(null);
    // Small delay to batch state updates
    const t = setTimeout(() => loadPage(true), 0);
    return () => clearTimeout(t);
  }, [customerId, eventType]); // eslint-disable-line react-hooks/exhaustive-deps

  const hasMore = continuation !== null;

  return (
    <div>
      {/* Filters */}
      <div className="flex gap-3 mb-4">
        <select value={customerId} onChange={e => setCustomerId(e.target.value)}
          className="px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
          <option value="">All customers</option>
          {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
        </select>
        <select value={eventType} onChange={e => setEventType(e.target.value)}
          className="px-3 py-1.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
          <option value="">All event types</option>
          {meters.map(m => <option key={m.slug} value={m.slug}>{m.name} ({m.slug})</option>)}
        </select>
        {(customerId || eventType) && (
          <button onClick={() => { setCustomerId(""); setEventType(""); }}
            className="px-3 py-1.5 text-sm text-gray-500 hover:text-gray-700">
            Clear filters
          </button>
        )}
        <div className="ml-auto text-sm text-gray-400 self-center">
          {events.length} events loaded{hasMore ? " (more available)" : ""}
        </div>
      </div>

      {/* Table */}
      <div className="bg-white rounded-xl border border-gray-200 shadow-sm overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="bg-gray-50 border-b border-gray-200">
              <th className="text-left px-4 py-2.5 font-medium text-gray-500 text-xs uppercase tracking-wider">Time</th>
              <th className="text-left px-4 py-2.5 font-medium text-gray-500 text-xs uppercase tracking-wider">Customer</th>
              <th className="text-left px-4 py-2.5 font-medium text-gray-500 text-xs uppercase tracking-wider">Event Type</th>
              <th className="text-right px-4 py-2.5 font-medium text-gray-500 text-xs uppercase tracking-wider">Value</th>
              <th className="text-left px-4 py-2.5 font-medium text-gray-500 text-xs uppercase tracking-wider">Idempotency Key</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-gray-100">
            {initialLoad && loading ? (
              <tr><td colSpan={5} className="px-4 py-12 text-center text-gray-400">
                <Loader2 className="w-5 h-5 animate-spin inline-block mr-2" />Loading events...
              </td></tr>
            ) : events.length === 0 ? (
              <tr><td colSpan={5} className="px-4 py-12 text-center text-gray-400">No events found</td></tr>
            ) : (
              events.map((evt, i) => (
                <tr key={`${evt.idempotencyKey}-${i}`} className="hover:bg-gray-50/50">
                  <td className="px-4 py-2 text-gray-900 whitespace-nowrap" title={formatTs(evt.timestampMs)}>
                    <div className="flex items-center gap-1.5">
                      <Clock className="w-3.5 h-3.5 text-gray-400" />
                      {timeAgo(evt.timestampMs)}
                    </div>
                  </td>
                  <td className="px-4 py-2 text-gray-700 font-mono text-xs">{evt.customerId}</td>
                  <td className="px-4 py-2">
                    <span className="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-indigo-50 text-indigo-700">
                      {evt.eventType || evt.meterSlug}
                    </span>
                  </td>
                  <td className="px-4 py-2 text-right font-mono text-gray-900">{evt.value.toString()}</td>
                  <td className="px-4 py-2 text-gray-400 font-mono text-xs truncate max-w-[200px]">{evt.idempotencyKey}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>

        {/* Load More */}
        {hasMore && !initialLoad && (
          <div className="border-t border-gray-100 px-4 py-3 text-center">
            <button onClick={() => loadPage(false)} disabled={loading}
              className="inline-flex items-center gap-2 px-4 py-2 text-sm font-medium text-indigo-600 hover:text-indigo-800 disabled:opacity-50">
              {loading ? <Loader2 className="w-4 h-4 animate-spin" /> : <ChevronDown className="w-4 h-4" />}
              {loading ? "Loading..." : "Load more"}
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

// --- Ingest + Benchmark Tab ---
interface BenchResult {
  accepted: number;
  duplicates: number;
  ingestMs: number;
  eventsPerSec: number;
  queryMs: number | null;
  usageTotal: string | null;
  error?: string;
}

function IngestTab({ customers, meters }: { customers: any[]; meters: any[] }) {
  const [customerId, setCustomerId] = useState("");
  const [eventType, setEventType] = useState("");
  const [value, setValue] = useState("1");
  const [count, setCount] = useState("100");
  const [result, setResult] = useState<BenchResult | null>(null);
  const [ingesting, setIngesting] = useState(false);

  async function handleIngest() {
    if (!customerId || !eventType) return;
    setIngesting(true);
    setResult(null);
    try {
      const n = parseInt(count) || 1;
      const v = parseInt(value) || 1;
      const batchSize = Math.min(n, 500);
      const batches = Math.ceil(n / batchSize);

      let totalAccepted = 0;
      let totalDuplicates = 0;
      const t0 = performance.now();

      // Fire batches concurrently for max throughput
      const promises = Array.from({ length: batches }, (_, batch) => {
        const size = batch === batches - 1 ? n - batch * batchSize : batchSize;
        const events = Array.from({ length: size }, (_, i) => ({
          customerId,
          eventType,
          value: BigInt(v),
          timestampMs: BigInt(Date.now() - Math.floor(Math.random() * 86400000)),
          idempotencyKey: `bench-${Date.now()}-${batch}-${i}-${Math.random().toString(36).slice(2)}`,
          propertiesJson: "",
        }));
        return eventClient.ingestEvents({ events });
      });

      const settled = await Promise.allSettled(promises);
      const ingestMs = Math.round(performance.now() - t0);
      let errors = 0;
      for (const s of settled) {
        if (s.status === "fulfilled") {
          totalAccepted += s.value.accepted;
          totalDuplicates += s.value.duplicates;
        } else {
          errors++;
          console.error("Batch failed:", s.reason);
        }
      }
      if (errors > 0) console.warn(`${errors}/${batches} batches failed`);

      // Now query usage — O(1) via atomic SUM index
      let queryMs: number | null = null;
      let usageTotal: string | null = null;
      try {
        const qt0 = performance.now();
        const usage = await eventClient.getUsage({
          customerId,
          meterSlug: eventType,
          startMs: BigInt(Date.now() - 30 * 86400000),
          endMs: BigInt(Date.now() + 86400000),
        });
        queryMs = Math.round((performance.now() - qt0) * 100) / 100;
        usageTotal = usage.totalValue.toString();
      } catch { /* ok */ }

      setResult({
        accepted: totalAccepted,
        duplicates: totalDuplicates,
        ingestMs,
        eventsPerSec: Math.round((totalAccepted / ingestMs) * 1000),
        queryMs,
        usageTotal,
      });
    } catch (e: any) {
      console.error("Ingest failed:", e);
      setResult({ accepted: 0, duplicates: 0, ingestMs: 0, eventsPerSec: 0, queryMs: null, usageTotal: null, error: e?.message || String(e) });
    }
    setIngesting(false);
  }

  return (
    <div className="bg-white rounded-xl border border-gray-200 p-6 shadow-sm">
      <h3 className="font-semibold text-gray-900 mb-1">Ingest + Benchmark</h3>
      <p className="text-sm text-gray-500 mb-5">
        Fire events at FDB Record Layer and measure throughput. After ingest, queries the SUM aggregate index — O(1) regardless of event count.
      </p>
      <div className="grid grid-cols-2 gap-4">
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Customer</label>
          <select value={customerId} onChange={e => setCustomerId(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
            <option value="">Select customer...</option>
            {customers.map(c => <option key={c.id} value={c.id}>{c.name} ({c.id.slice(0,12)}...)</option>)}
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
        <Zap className="w-4 h-4" /> {ingesting ? "Ingesting..." : "Run Benchmark"}
      </button>

      {result && (
        <div className="mt-5 space-y-4">
          {/* Ingest results */}
          <div className="grid grid-cols-4 gap-3">
            <div className="p-4 bg-emerald-50 rounded-lg border border-emerald-200">
              <div className="text-2xl font-bold text-emerald-700">{result.accepted.toLocaleString()}</div>
              <div className="text-xs text-emerald-600 mt-0.5">Events Accepted</div>
            </div>
            <div className="p-4 bg-blue-50 rounded-lg border border-blue-200">
              <div className="text-2xl font-bold text-blue-700">{result.ingestMs.toLocaleString()}ms</div>
              <div className="text-xs text-blue-600 mt-0.5">Ingest Time</div>
            </div>
            <div className="p-4 bg-indigo-50 rounded-lg border border-indigo-200">
              <div className="text-2xl font-bold text-indigo-700">{result.eventsPerSec.toLocaleString()}</div>
              <div className="text-xs text-indigo-600 mt-0.5">Events/sec</div>
            </div>
            {result.duplicates > 0 && (
              <div className="p-4 bg-amber-50 rounded-lg border border-amber-200">
                <div className="text-2xl font-bold text-amber-700">{result.duplicates}</div>
                <div className="text-xs text-amber-600 mt-0.5">Duplicates (deduped)</div>
              </div>
            )}
          </div>

          {result.error && (
            <div className="p-3 bg-red-50 rounded-lg border border-red-200 text-sm text-red-700">
              Error: {result.error}
            </div>
          )}

          {/* Query result — the O(1) demo */}
          {result.queryMs !== null && (
            <div className="p-4 bg-gray-900 rounded-lg text-white">
              <div className="flex items-center gap-2 mb-2">
                <Zap className="w-4 h-4 text-yellow-400" />
                <span className="text-xs font-semibold uppercase tracking-wider text-gray-400">O(1) Aggregation Query</span>
              </div>
              <div className="grid grid-cols-2 gap-6">
                <div>
                  <div className="text-3xl font-bold text-yellow-400">{result.queryMs}ms</div>
                  <div className="text-xs text-gray-400 mt-1">
                    Query time — same whether 100 events or 100 million.
                    <br />FDB atomic SUM index: single key read, no scanning.
                  </div>
                </div>
                <div>
                  <div className="text-3xl font-bold text-white">{Number(result.usageTotal).toLocaleString()}</div>
                  <div className="text-xs text-gray-400 mt-1">
                    Total usage value — pre-computed by FDB Record Layer.
                    <br />Updated atomically on every event ingest.
                  </div>
                </div>
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// --- Query Tab ---
function QueryTab({ customers, meters }: { customers: any[]; meters: any[] }) {
  const [customerId, setCustomerId] = useState("");
  const [meterSlug, setMeterSlug] = useState("");
  const [hours, setHours] = useState("24");
  const [result, setResult] = useState<{ total: string } | null>(null);
  const [querying, setQuerying] = useState(false);

  async function handleQuery() {
    if (!customerId || !meterSlug) return;
    setQuerying(true);
    try {
      const h = parseInt(hours) || 24;
      const now = Date.now();
      const start = now - h * 3600_000;
      const resp = await eventClient.getUsage({
        customerId,
        meterSlug,
        startMs: BigInt(start),
        endMs: BigInt(now),
      });
      setResult({ total: resp.totalValue.toString() });
    } catch {
      setResult({ total: "error" });
    }
    setQuerying(false);
  }

  return (
    <div className="bg-white rounded-xl border border-gray-200 p-6 shadow-sm">
      <h3 className="font-semibold text-gray-900 mb-1">Query Aggregated Usage</h3>
      <p className="text-sm text-gray-500 mb-5">Read SUM aggregates from atomic indexes -- O(1) reads regardless of event count.</p>
      <div className="grid grid-cols-3 gap-4">
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Customer</label>
          <select value={customerId} onChange={e => setCustomerId(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
            <option value="">Select customer...</option>
            {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Meter</label>
          <select value={meterSlug} onChange={e => setMeterSlug(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
            <option value="">Select meter...</option>
            {meters.map(m => <option key={m.slug} value={m.slug}>{m.name} ({m.slug})</option>)}
          </select>
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700 mb-1">Time range</label>
          <select value={hours} onChange={e => setHours(e.target.value)}
            className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
            <option value="1">Last 1 hour</option>
            <option value="6">Last 6 hours</option>
            <option value="24">Last 24 hours</option>
            <option value="168">Last 7 days</option>
            <option value="720">Last 30 days</option>
          </select>
        </div>
      </div>
      <button onClick={handleQuery} disabled={querying || !customerId || !meterSlug}
        className="mt-5 flex items-center gap-2 px-5 py-2.5 bg-gray-900 text-white rounded-lg text-sm font-medium hover:bg-gray-800 disabled:opacity-50 disabled:cursor-not-allowed">
        <Search className="w-4 h-4" /> {querying ? "Querying..." : "Query Usage"}
      </button>
      {result && (
        <div className="mt-5 flex gap-4">
          <div className="flex-1 p-5 bg-indigo-50 rounded-lg border border-indigo-200">
            <div className="flex items-center gap-2 text-indigo-600 mb-1">
              <Activity className="w-4 h-4" />
              <span className="text-xs font-medium uppercase tracking-wider">Total Value</span>
            </div>
            <div className="text-3xl font-bold text-indigo-900">{result.total}</div>
          </div>
        </div>
      )}
    </div>
  );
}

// --- Main Page ---
export function EventsPage() {
  const [tab, setTab] = useState<"browse" | "ingest" | "query">("browse");
  const [customers, setCustomers] = useState<any[]>([]);
  const [meters, setMeters] = useState<any[]>([]);

  useEffect(() => {
    customerClient.listCustomers({}).then(r => setCustomers(r.customers)).catch(() => {});
    meterClient.listMeters({}).then(r => setMeters(r.meters)).catch(() => {});
  }, []);

  return (
    <div className="p-8 max-w-6xl">
      <div className="mb-6">
        <h1 className="text-2xl font-bold text-gray-900">Events</h1>
        <p className="text-sm text-gray-500 mt-1">Browse, ingest, and query usage events.</p>
      </div>

      {/* Tabs */}
      <div className="flex gap-1 bg-gray-100 rounded-lg p-1 mb-6 w-fit">
        <button onClick={() => setTab("browse")}
          className={`flex items-center gap-2 px-4 py-2 rounded-md text-sm font-medium transition-colors ${tab === "browse" ? "bg-white shadow-sm text-gray-900" : "text-gray-500 hover:text-gray-700"}`}>
          <List className="w-4 h-4" /> Browse
        </button>
        <button onClick={() => setTab("ingest")}
          className={`flex items-center gap-2 px-4 py-2 rounded-md text-sm font-medium transition-colors ${tab === "ingest" ? "bg-white shadow-sm text-gray-900" : "text-gray-500 hover:text-gray-700"}`}>
          <Send className="w-4 h-4" /> Ingest
        </button>
        <button onClick={() => setTab("query")}
          className={`flex items-center gap-2 px-4 py-2 rounded-md text-sm font-medium transition-colors ${tab === "query" ? "bg-white shadow-sm text-gray-900" : "text-gray-500 hover:text-gray-700"}`}>
          <BarChart3 className="w-4 h-4" /> Query
        </button>
      </div>

      {tab === "browse" && <BrowseTab customers={customers} meters={meters} />}
      {tab === "ingest" && <IngestTab customers={customers} meters={meters} />}
      {tab === "query" && <QueryTab customers={customers} meters={meters} />}
    </div>
  );
}
