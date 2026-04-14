import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { PlanService } from "@/gen/metrognome/v1/plan_pb";
import type { Plan } from "@/gen/metrognome/v1/plan_pb";
import { formatTimestamp } from "@/lib/utils";

export function PlansPage() {
  const [plans, setPlans] = useState<Plan[]>([]);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  const client = createClient(PlanService, transport);

  async function load() {
    const resp = await client.listPlans({});
    setPlans(resp.plans);
  }

  useEffect(() => { load(); }, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    await client.createPlan({ name, description });
    setName("");
    setDescription("");
    load();
  }

  return (
    <div>
      <h2 className="text-2xl font-bold mb-6">Plans</h2>

      <form onSubmit={handleCreate} className="mb-8 flex gap-4">
        <input
          type="text"
          placeholder="Plan name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 flex-1"
          required
        />
        <input
          type="text"
          placeholder="Description"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 flex-1"
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
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Name</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Description</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">ID</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Created</th>
            </tr>
          </thead>
          <tbody>
            {plans.map((p) => (
              <tr key={p.id} className="border-b border-[var(--color-border)] last:border-0">
                <td className="p-4 font-medium">{p.name}</td>
                <td className="p-4 text-sm text-[var(--color-muted-foreground)]">{p.description || "-"}</td>
                <td className="p-4 text-xs font-mono text-[var(--color-muted-foreground)]">{p.id}</td>
                <td className="p-4 text-sm">{formatTimestamp(p.createdAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
