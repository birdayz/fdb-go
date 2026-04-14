import { useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { EventService } from "@/gen/metrognome/v1/event_pb";
import { formatCents } from "@/lib/utils";

export function EventsPage() {
  const [customerId, setCustomerId] = useState("");
  const [eventType, setEventType] = useState("");
  const [value, setValue] = useState("1");
  const [count, setCount] = useState("10");
  const [result, setResult] = useState<{ accepted: number; duplicates: number } | null>(null);

  // Usage query
  const [queryCustomerId, setQueryCustomerId] = useState("");
  const [queryMeterSlug, setQueryMeterSlug] = useState("");
  const [usageResult, setUsageResult] = useState<bigint | null>(null);

  const client = createClient(EventService, transport);

  async function handleIngest(e: React.FormEvent) {
    e.preventDefault();
    const n = parseInt(count);
    const v = parseInt(value);
    const events = Array.from({ length: n }, (_, i) => ({
      customerId,
      eventType,
      value: BigInt(v),
      timestampMs: BigInt(Date.now()),
      idempotencyKey: `ui-${Date.now()}-${i}`,
      propertiesJson: "",
    }));

    const resp = await client.ingestEvents({ events });
    setResult({ accepted: resp.accepted, duplicates: resp.duplicates });
  }

  async function handleQuery(e: React.FormEvent) {
    e.preventDefault();
    const now = Date.now();
    const dayAgo = now - 24 * 60 * 60 * 1000;
    const resp = await client.getUsage({
      customerId: queryCustomerId,
      meterSlug: queryMeterSlug,
      startMs: BigInt(dayAgo),
      endMs: BigInt(now),
    });
    setUsageResult(resp.totalValue);
  }

  return (
    <div>
      <h2 className="text-2xl font-bold mb-6">Events</h2>

      <div className="grid grid-cols-2 gap-8">
        {/* Ingest */}
        <div className="bg-white rounded-xl border border-[var(--color-border)] p-6">
          <h3 className="font-bold mb-4">Ingest Events</h3>
          <form onSubmit={handleIngest} className="space-y-4">
            <input
              type="text"
              placeholder="Customer ID"
              value={customerId}
              onChange={(e) => setCustomerId(e.target.value)}
              className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-full"
              required
            />
            <input
              type="text"
              placeholder="Event type (meter slug)"
              value={eventType}
              onChange={(e) => setEventType(e.target.value)}
              className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-full"
              required
            />
            <div className="flex gap-4">
              <input
                type="number"
                placeholder="Value per event"
                value={value}
                onChange={(e) => setValue(e.target.value)}
                className="border border-[var(--color-border)] rounded-lg px-4 py-2 flex-1"
              />
              <input
                type="number"
                placeholder="Count"
                value={count}
                onChange={(e) => setCount(e.target.value)}
                className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-24"
              />
            </div>
            <button
              type="submit"
              className="bg-[var(--color-primary)] text-white px-6 py-2 rounded-lg hover:opacity-90 w-full"
            >
              Ingest
            </button>
          </form>
          {result && (
            <div className="mt-4 p-4 bg-[var(--color-muted)] rounded-lg text-sm">
              Accepted: {result.accepted} | Duplicates: {result.duplicates}
            </div>
          )}
        </div>

        {/* Query */}
        <div className="bg-white rounded-xl border border-[var(--color-border)] p-6">
          <h3 className="font-bold mb-4">Query Usage (last 24h)</h3>
          <form onSubmit={handleQuery} className="space-y-4">
            <input
              type="text"
              placeholder="Customer ID"
              value={queryCustomerId}
              onChange={(e) => setQueryCustomerId(e.target.value)}
              className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-full"
              required
            />
            <input
              type="text"
              placeholder="Meter slug"
              value={queryMeterSlug}
              onChange={(e) => setQueryMeterSlug(e.target.value)}
              className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-full"
              required
            />
            <button
              type="submit"
              className="bg-[var(--color-foreground)] text-white px-6 py-2 rounded-lg hover:opacity-90 w-full"
            >
              Query
            </button>
          </form>
          {usageResult !== null && (
            <div className="mt-4 p-4 bg-[var(--color-muted)] rounded-lg">
              <p className="text-sm text-[var(--color-muted-foreground)]">Total Usage</p>
              <p className="text-3xl font-bold">{usageResult.toString()}</p>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
