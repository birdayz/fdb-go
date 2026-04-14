import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import type { Meter } from "@/gen/metrognome/v1/meter_pb";
import { formatTimestamp } from "@/lib/utils";

const aggTypes: Record<number, string> = {
  1: "COUNT",
  2: "SUM",
  3: "MAX",
  4: "UNIQUE",
  5: "LATEST",
};

export function MetersPage() {
  const [meters, setMeters] = useState<Meter[]>([]);
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [aggType, setAggType] = useState(2); // SUM
  const [groupBy, setGroupBy] = useState("");

  const client = createClient(MeterService, transport);

  async function load() {
    const resp = await client.listMeters({});
    setMeters(resp.meters);
  }

  useEffect(() => { load(); }, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    const groupByProps = groupBy ? groupBy.split(",").map((s) => s.trim()) : [];
    await client.createMeter({
      slug,
      name,
      aggregationType: aggType,
      groupByProperties: groupByProps,
    });
    setSlug("");
    setName("");
    setGroupBy("");
    load();
  }

  return (
    <div>
      <h2 className="text-2xl font-bold mb-6">Meters</h2>

      <form onSubmit={handleCreate} className="mb-8 flex gap-4 flex-wrap">
        <input
          type="text"
          placeholder="Slug (e.g. api_calls)"
          value={slug}
          onChange={(e) => setSlug(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-48"
          required
        />
        <input
          type="text"
          placeholder="Display name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 flex-1"
          required
        />
        <select
          value={aggType}
          onChange={(e) => setAggType(Number(e.target.value))}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2"
        >
          <option value={1}>COUNT</option>
          <option value={2}>SUM</option>
          <option value={3}>MAX</option>
        </select>
        <input
          type="text"
          placeholder="Group by (comma-separated)"
          value={groupBy}
          onChange={(e) => setGroupBy(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-64"
        />
        <button
          type="submit"
          className="bg-[var(--color-primary)] text-white px-6 py-2 rounded-lg hover:opacity-90"
        >
          Create
        </button>
      </form>

      <div className="bg-white rounded-xl border border-[var(--color-border)]">
        <table className="w-full">
          <thead>
            <tr className="border-b border-[var(--color-border)]">
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Slug</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Name</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Type</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Group By</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Created</th>
            </tr>
          </thead>
          <tbody>
            {meters.map((m) => (
              <tr key={m.id} className="border-b border-[var(--color-border)] last:border-0">
                <td className="p-4 font-mono text-sm font-medium">{m.slug}</td>
                <td className="p-4">{m.name}</td>
                <td className="p-4">
                  <span className="bg-[var(--color-muted)] px-2 py-1 rounded text-xs font-medium">
                    {aggTypes[m.aggregationType] || "?"}
                  </span>
                </td>
                <td className="p-4 text-sm text-[var(--color-muted-foreground)]">
                  {m.groupByProperties.length > 0 ? m.groupByProperties.join(", ") : "-"}
                </td>
                <td className="p-4 text-sm">{formatTimestamp(m.createdAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
