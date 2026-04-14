import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import type { Customer } from "@/gen/metrognome/v1/customer_pb";
import { formatTimestamp } from "@/lib/utils";

export function CustomersPage() {
  const [customers, setCustomers] = useState<Customer[]>([]);
  const [name, setName] = useState("");
  const [externalId, setExternalId] = useState("");

  const client = createClient(CustomerService, transport);

  async function load() {
    const resp = await client.listCustomers({});
    setCustomers(resp.customers);
  }

  useEffect(() => { load(); }, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    await client.createCustomer({ name, externalId });
    setName("");
    setExternalId("");
    load();
  }

  return (
    <div>
      <h2 className="text-2xl font-bold mb-6">Customers</h2>

      <form onSubmit={handleCreate} className="mb-8 flex gap-4">
        <input
          type="text"
          placeholder="Customer name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 flex-1"
          required
        />
        <input
          type="text"
          placeholder="External ID (optional)"
          value={externalId}
          onChange={(e) => setExternalId(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 w-48"
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
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">External ID</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">ID</th>
              <th className="text-left p-4 text-sm font-medium text-[var(--color-muted-foreground)]">Created</th>
            </tr>
          </thead>
          <tbody>
            {customers.map((c) => (
              <tr key={c.id} className="border-b border-[var(--color-border)] last:border-0">
                <td className="p-4 font-medium">{c.name}</td>
                <td className="p-4 text-sm text-[var(--color-muted-foreground)]">{c.externalId || "-"}</td>
                <td className="p-4 text-xs font-mono text-[var(--color-muted-foreground)]">{c.id}</td>
                <td className="p-4 text-sm">{formatTimestamp(c.createdAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
