import { useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { InvoiceService } from "@/gen/metrognome/v1/invoice_pb";
import type { Invoice } from "@/gen/metrognome/v1/invoice_pb";
import { formatCents, formatDate } from "@/lib/utils";

const statusLabels: Record<number, { label: string; color: string }> = {
  1: { label: "Draft", color: "bg-gray-200 text-gray-800" },
  2: { label: "Issued", color: "bg-blue-100 text-blue-800" },
  3: { label: "Paid", color: "bg-green-100 text-green-800" },
  4: { label: "Void", color: "bg-red-100 text-red-800" },
};

export function InvoicesPage() {
  const [customerId, setCustomerId] = useState("");
  const [invoices, setInvoices] = useState<Invoice[]>([]);
  const [loaded, setLoaded] = useState(false);

  const client = createClient(InvoiceService, transport);

  async function handleSearch(e: React.FormEvent) {
    e.preventDefault();
    const resp = await client.listInvoices({ customerId });
    setInvoices(resp.invoices);
    setLoaded(true);
  }

  return (
    <div>
      <h2 className="text-2xl font-bold mb-6">Invoices</h2>

      <form onSubmit={handleSearch} className="mb-8 flex gap-4">
        <input
          type="text"
          placeholder="Customer ID"
          value={customerId}
          onChange={(e) => setCustomerId(e.target.value)}
          className="border border-[var(--color-border)] rounded-lg px-4 py-2 flex-1"
          required
        />
        <button
          type="submit"
          className="bg-[var(--color-foreground)] text-white px-6 py-2 rounded-lg hover:opacity-90"
        >
          Search
        </button>
      </form>

      {loaded && invoices.length === 0 && (
        <p className="text-[var(--color-muted-foreground)]">No invoices found.</p>
      )}

      {invoices.map((inv) => {
        const status = statusLabels[inv.status] || { label: "Unknown", color: "bg-gray-100" };
        return (
          <div key={inv.id} className="bg-white rounded-xl border border-[var(--color-border)] p-6 mb-4">
            <div className="flex justify-between items-start mb-4">
              <div>
                <p className="text-xs font-mono text-[var(--color-muted-foreground)]">{inv.id}</p>
                <p className="text-sm mt-1">
                  {formatDate(inv.periodStart)} - {formatDate(inv.periodEnd)}
                </p>
              </div>
              <span className={`px-3 py-1 rounded-full text-xs font-medium ${status.color}`}>
                {status.label}
              </span>
            </div>

            <table className="w-full text-sm mb-4">
              <thead>
                <tr className="border-b border-[var(--color-border)]">
                  <th className="text-left py-2 text-[var(--color-muted-foreground)]">Description</th>
                  <th className="text-right py-2 text-[var(--color-muted-foreground)]">Qty</th>
                  <th className="text-right py-2 text-[var(--color-muted-foreground)]">Amount</th>
                </tr>
              </thead>
              <tbody>
                {inv.lineItems.map((li, i) => (
                  <tr key={i} className="border-b border-[var(--color-border)]">
                    <td className="py-2">{li.description || li.meterSlug}</td>
                    <td className="py-2 text-right">{li.quantity.toString()}</td>
                    <td className="py-2 text-right font-medium">{formatCents(Number(li.amountCents))}</td>
                  </tr>
                ))}
              </tbody>
            </table>

            <div className="flex justify-end gap-8 text-sm">
              <div>
                <span className="text-[var(--color-muted-foreground)]">Subtotal: </span>
                <span className="font-medium">{formatCents(Number(inv.subtotalCents))}</span>
              </div>
              {Number(inv.creditsAppliedCents) > 0 && (
                <div>
                  <span className="text-[var(--color-muted-foreground)]">Credits: </span>
                  <span className="font-medium text-[var(--color-success)]">
                    -{formatCents(Number(inv.creditsAppliedCents))}
                  </span>
                </div>
              )}
              <div>
                <span className="text-[var(--color-muted-foreground)]">Total: </span>
                <span className="text-lg font-bold">{formatCents(Number(inv.totalCents))}</span>
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}
