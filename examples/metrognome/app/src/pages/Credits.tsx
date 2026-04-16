import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CreditService } from "@/gen/metrognome/v1/credit_pb";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { CreditCard, Plus, Search, Wallet } from "lucide-react";

const creditClient = createClient(CreditService, transport);
const customerClient = createClient(CustomerService, transport);

function formatCents(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

export function CreditsPage() {
  const [customers, setCustomers] = useState<any[]>([]);
  const [customerId, setCustomerId] = useState("");
  const [credits, setCredits] = useState<any[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [showGrant, setShowGrant] = useState(false);
  const [grantCustomerId, setGrantCustomerId] = useState("");
  const [grantAmount, setGrantAmount] = useState("");
  const [grantPriority, setGrantPriority] = useState("0");

  useEffect(() => {
    customerClient.listCustomers({}).then(r => setCustomers(r.customers)).catch(() => {});
  }, []);

  async function search() {
    if (!customerId) return;
    try {
      const resp = await creditClient.listCredits({ customerId });
      setCredits(resp.credits);
    } catch (e) { setCredits([]); }
    setLoaded(true);
  }

  async function grant() {
    if (!grantCustomerId || !grantAmount) return;
    const cents = Math.round(parseFloat(grantAmount) * 100);
    await creditClient.grantCredit({
      customerId: grantCustomerId,
      amountCents: BigInt(cents),
      priority: parseInt(grantPriority) || 0,
    });
    setShowGrant(false); setGrantAmount(""); setGrantPriority("0");
    if (grantCustomerId === customerId) search();
  }

  return (
    <div className="p-8 max-w-5xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Credits</h1>
          <p className="text-sm text-gray-500 mt-1">Prepaid balances and free usage allowances. Applied to invoices by priority.</p>
        </div>
        <button onClick={() => setShowGrant(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Grant Credit
        </button>
      </div>

      {showGrant && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">Grant Credit</h3>
          <div className="grid grid-cols-3 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Customer</label>
              <select value={grantCustomerId} onChange={e => setGrantCustomerId(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select...</option>
                {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Amount ($)</label>
              <input type="number" step="0.01" value={grantAmount} onChange={e => setGrantAmount(e.target.value)} placeholder="100.00"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Priority (lower = first)</label>
              <input type="number" value={grantPriority} onChange={e => setGrantPriority(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
          </div>
          <div className="flex gap-2 mt-4">
            <button onClick={grant} className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700">Grant</button>
            <button onClick={() => setShowGrant(false)} className="px-4 py-2 text-gray-600 rounded-lg text-sm hover:bg-gray-100">Cancel</button>
          </div>
        </div>
      )}

      <div className="bg-white rounded-xl border border-gray-200 p-5 mb-6 shadow-sm">
        <div className="flex gap-3">
          <select value={customerId} onChange={e => setCustomerId(e.target.value)}
            className="flex-1 px-3 py-2.5 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
            <option value="">Select customer...</option>
            {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
          </select>
          <button onClick={search} disabled={!customerId}
            className="flex items-center gap-2 px-5 py-2.5 bg-gray-900 text-white rounded-lg text-sm font-medium hover:bg-gray-800 disabled:opacity-50">
            <Search className="w-4 h-4" /> View Credits
          </button>
        </div>
      </div>

      {!loaded ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Wallet className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">Select a customer to view their credits</p>
        </div>
      ) : credits.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <CreditCard className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No credits found</p>
          <p className="text-sm text-gray-400 mt-1">Grant a credit to this customer.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {credits.map((cr: any) => {
            const remaining = Number(cr.remainingCents);
            const total = Number(cr.amountCents);
            const pct = total > 0 ? Math.round((remaining / total) * 100) : 0;
            return (
              <div key={cr.id} className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
                <div className="flex items-center justify-between mb-3">
                  <div className="flex items-center gap-3">
                    <div className="w-10 h-10 rounded-lg bg-emerald-50 flex items-center justify-center">
                      <CreditCard className="w-5 h-5 text-emerald-600" />
                    </div>
                    <div>
                      <div className="text-sm font-medium text-gray-900">Credit Grant</div>
                      <div className="text-xs text-gray-400 font-mono">{cr.id}</div>
                    </div>
                  </div>
                  <div className="text-right">
                    <div className="text-lg font-bold text-gray-900">{formatCents(remaining)}</div>
                    <div className="text-xs text-gray-400">of {formatCents(total)} remaining</div>
                  </div>
                </div>
                <div className="w-full bg-gray-100 rounded-full h-2">
                  <div className={`h-2 rounded-full transition-all ${pct > 50 ? "bg-emerald-500" : pct > 20 ? "bg-amber-500" : "bg-red-500"}`}
                    style={{ width: `${pct}%` }} />
                </div>
                <div className="flex gap-4 mt-3 text-xs text-gray-500">
                  <span>Priority: <strong>{cr.priority}</strong></span>
                  {cr.expiresAt && Number(cr.expiresAt) > 0 && (
                    <span>Expires: <strong>{new Date(Number(cr.expiresAt)).toLocaleDateString()}</strong></span>
                  )}
                  <span className="ml-auto">Created {new Date(Number(cr.createdAt)).toLocaleDateString()}</span>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
