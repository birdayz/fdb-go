import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import type { Meter } from "@/gen/metrognome/v1/meter_pb";
import { Gauge, Plus, Hash, Sigma, TrendingUp, Fingerprint, Clock } from "lucide-react";

const client = createClient(MeterService, transport);

const aggLabels: Record<number, { label: string; icon: typeof Sigma; color: string }> = {
  1: { label: "COUNT", icon: Hash, color: "bg-blue-100 text-blue-700" },
  2: { label: "SUM", icon: Sigma, color: "bg-purple-100 text-purple-700" },
  3: { label: "MAX", icon: TrendingUp, color: "bg-amber-100 text-amber-700" },
  4: { label: "UNIQUE", icon: Fingerprint, color: "bg-emerald-100 text-emerald-700" },
  5: { label: "LATEST", icon: Clock, color: "bg-rose-100 text-rose-700" },
};

export function MetersPage() {
  const [meters, setMeters] = useState<Meter[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [aggType, setAggType] = useState(2);
  const [groupBy, setGroupBy] = useState("");
  const [eventTypeFilter, setEventTypeFilter] = useState("");
  const [valueProperty, setValueProperty] = useState("");

  async function load() {
    try {
      const resp = await client.listMeters({});
      setMeters(resp.meters);
    } catch (e) { console.error(e); }
    setLoading(false);
  }

  useEffect(() => { load(); }, []);

  async function handleCreate() {
    if (!slug.trim() || !name.trim()) return;
    const groupByProps = groupBy ? groupBy.split(",").map(s => s.trim()).filter(Boolean) : [];
    await client.createMeter({
      slug, name, aggregationType: aggType,
      groupByProperties: groupByProps,
      eventTypeFilter: eventTypeFilter || undefined,
      valueProperty: valueProperty || undefined,
    });
    setSlug(""); setName(""); setGroupBy(""); setEventTypeFilter(""); setValueProperty("");
    setShowCreate(false);
    load();
  }

  return (
    <div className="p-8 max-w-6xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Billable Metrics</h1>
          <p className="text-sm text-gray-500 mt-1">Define how raw events are filtered, grouped, and aggregated into quantities.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Create Metric
        </button>
      </div>

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New Billable Metric</h3>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Slug <span className="text-red-400">*</span></label>
              <input value={slug} onChange={e => setSlug(e.target.value)} placeholder="e.g. api_calls"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm font-mono focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Display Name <span className="text-red-400">*</span></label>
              <input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. API Calls"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Aggregation Type</label>
              <select value={aggType} onChange={e => setAggType(Number(e.target.value))}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value={1}>COUNT — count events</option>
                <option value={2}>SUM — sum a property value</option>
                <option value={3}>MAX — max of a property value</option>
                <option value={4}>UNIQUE — count distinct values</option>
                <option value={5}>LATEST — last value per group</option>
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Value Property</label>
              <input value={valueProperty} onChange={e => setValueProperty(e.target.value)} placeholder="e.g. bytes (for SUM/MAX)"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Group By Properties</label>
              <input value={groupBy} onChange={e => setGroupBy(e.target.value)} placeholder="e.g. region, model (comma-separated)"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Event Type Filter</label>
              <input value={eventTypeFilter} onChange={e => setEventTypeFilter(e.target.value)} placeholder="Filter by event_type (optional)"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
          </div>
          <div className="flex gap-2 mt-4">
            <button onClick={handleCreate} className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700">Create</button>
            <button onClick={() => setShowCreate(false)} className="px-4 py-2 text-gray-600 rounded-lg text-sm hover:bg-gray-100">Cancel</button>
          </div>
        </div>
      )}

      {loading ? (
        <div className="text-center py-12 text-gray-400">Loading...</div>
      ) : meters.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Gauge className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No billable metrics yet</p>
          <p className="text-sm text-gray-400 mt-1">Create a metric to start aggregating usage events.</p>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-3">
          {meters.map(m => {
            const agg = aggLabels[m.aggregationType] || { label: "?", icon: Hash, color: "bg-gray-100 text-gray-600" };
            return (
              <div key={m.id} className="bg-white rounded-xl border border-gray-200 p-5 hover:shadow-sm transition-shadow">
                <div className="flex items-start justify-between">
                  <div className="flex items-center gap-3">
                    <div className="w-10 h-10 rounded-lg bg-emerald-50 flex items-center justify-center">
                      <Gauge className="w-5 h-5 text-emerald-600" />
                    </div>
                    <div>
                      <h3 className="font-semibold text-gray-900">{m.name}</h3>
                      <code className="text-xs text-gray-400 font-mono">{m.slug}</code>
                    </div>
                  </div>
                  <span className={`inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-semibold ${agg.color}`}>
                    <agg.icon className="w-3 h-3" /> {agg.label}
                  </span>
                </div>
                <div className="flex gap-4 mt-3 text-xs text-gray-500">
                  {m.groupByProperties.length > 0 && (
                    <span>Group by: <strong className="text-gray-700">{m.groupByProperties.join(", ")}</strong></span>
                  )}
                  {m.eventTypeFilter && (
                    <span>Filter: <strong className="text-gray-700">{m.eventTypeFilter}</strong></span>
                  )}
                  {m.valueProperty && (
                    <span>Value: <strong className="text-gray-700">{m.valueProperty}</strong></span>
                  )}
                  <span className="ml-auto">{new Date(Number(m.createdAt)).toLocaleDateString()}</span>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
