import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { InvoiceService } from "@/gen/metrognome/v1/invoice_pb";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import type { Invoice } from "@/gen/metrognome/v1/invoice_pb";
import { FileText, Search, ChevronDown, ChevronUp, Receipt } from "lucide-react";

const invoiceClient = createClient(InvoiceService, transport);
const customerClient = createClient(CustomerService, transport);

const statusLabels: Record<number, { label: string; color: string }> = {
  1: { label: "Draft", color: "bg-yellow-100 text-yellow-800 border-yellow-200" },
  2: { label: "Issued", color: "bg-blue-100 text-blue-800 border-blue-200" },
  3: { label: "Paid", color: "bg-emerald-100 text-emerald-800 border-emerald-200" },
  4: { label: "Void", color: "bg-red-100 text-red-800 border-red-200" },
};

function formatCents(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

export function InvoicesPage() {
  const [customers, setCustomers] = useState<any[]>([]);
  const [customerId, setCustomerId] = useState("");
  const [invoices, setInvoices] = useState<Invoice[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    customerClient.listCustomers({}).then(r => setCustomers(r.customers)).catch(() => {});
  }, []);

  async function handleSearch() {
    if (!customerId) return;
    setLoading(true);
    try {
      const resp = await invoiceClient.listInvoices({ customerId });
      setInvoices(resp.invoices);
    } catch (e) { setInvoices([]); }
    setLoaded(true);
    setLoading(false);
  }

  return (
    <div className="p-8 max-w-5xl">
      <div className="mb-6">
        <h1 className="text-2xl font-bold text-gray-900">Invoices</h1>
        <p className="text-sm text-gray-500 mt-1">View billing documents with line-item breakdowns and credit application.</p>
      </div>

      <div className="bg-white rounded-xl border border-gray-200 p-5 mb-6 shadow-sm">
        <div className="flex gap-3">
          <select value={customerId} onChange={e => setCustomerId(e.target.value)}
            className="flex-1 px-3 py-2.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
            <option value="">Select customer...</option>
            {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
          <button onClick={handleSearch} disabled={!customerId || loading}
            className="flex items-center gap-2 px-5 py-2.5 bg-gray-900 text-white rounded-lg text-sm font-medium hover:bg-gray-800 disabled:opacity-50">
            <Search className="w-4 h-4" /> {loading ? "Loading..." : "Search"}
          </button>
        </div>
      </div>

      {!loaded ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <FileText className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">Select a customer to view invoices</p>
        </div>
      ) : invoices.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Receipt className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No invoices found</p>
          <p className="text-sm text-gray-400 mt-1">Generate an invoice from the billing engine first.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {invoices.map(inv => {
            const status = statusLabels[inv.status] || { label: "Unknown", color: "bg-gray-100 text-gray-600" };
            const isExpanded = expanded === inv.id;
            return (
              <div key={inv.id} className="bg-white rounded-xl border border-gray-200 overflow-hidden shadow-sm">
                <button onClick={() => setExpanded(isExpanded ? null : inv.id)}
                  className="w-full flex items-center justify-between px-5 py-4 hover:bg-gray-50/50 transition-colors">
                  <div className="flex items-center gap-4">
                    <div className="w-10 h-10 rounded-lg bg-gray-50 flex items-center justify-center">
                      <FileText className="w-5 h-5 text-gray-400" />
                    </div>
                    <div className="text-left">
                      <div className="text-sm font-medium text-gray-900">
                        {new Date(Number(inv.periodStart)).toLocaleDateString("en-US", { month: "short", day: "numeric" })} — {new Date(Number(inv.periodEnd)).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" })}
                      </div>
                      <div className="text-xs text-gray-400 font-mono mt-0.5">{inv.id}</div>
                    </div>
                  </div>
                  <div className="flex items-center gap-4">
                    <span className={`px-2.5 py-1 rounded-full text-xs font-semibold border ${status.color}`}>
                      {status.label}
                    </span>
                    <span className="text-lg font-bold text-gray-900">{formatCents(Number(inv.totalCents))}</span>
                    {isExpanded ? <ChevronUp className="w-4 h-4 text-gray-400" /> : <ChevronDown className="w-4 h-4 text-gray-400" />}
                  </div>
                </button>

                {isExpanded && (
                  <div className="border-t border-gray-100 px-5 py-4">
                    <table className="w-full text-sm">
                      <thead>
                        <tr className="border-b border-gray-100">
                          <th className="text-left py-2 px-1">Description</th>
                          <th className="text-right py-2 px-1">Quantity</th>
                          <th className="text-right py-2 px-1">Amount</th>
                        </tr>
                      </thead>
                      <tbody>
                        {inv.lineItems.map((li, i) => (
                          <tr key={i} className="border-b border-gray-50">
                            <td className="py-2 px-1 text-gray-700">{li.description || li.meterSlug || "—"}</td>
                            <td className="py-2 px-1 text-right text-gray-500">{li.quantity.toString()}</td>
                            <td className="py-2 px-1 text-right font-medium text-gray-900">{formatCents(Number(li.amountCents))}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>

                    <div className="flex justify-end gap-6 mt-4 pt-3 border-t border-gray-100 text-sm">
                      <div><span className="text-gray-500">Subtotal</span> <span className="font-medium ml-2">{formatCents(Number(inv.subtotalCents))}</span></div>
                      {Number(inv.creditsAppliedCents) > 0 && (
                        <div><span className="text-gray-500">Credits</span> <span className="font-medium text-emerald-600 ml-2">-{formatCents(Number(inv.creditsAppliedCents))}</span></div>
                      )}
                      <div><span className="text-gray-500">Total</span> <span className="font-bold text-lg ml-2">{formatCents(Number(inv.totalCents))}</span></div>
                    </div>
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
