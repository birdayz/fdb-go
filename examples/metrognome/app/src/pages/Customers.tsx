import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import type { Customer } from "@/gen/metrognome/v1/customer_pb";
import { Users, Plus, Search, ExternalLink, Copy, Check, ChevronRight } from "lucide-react";
import { useNavigate } from "react-router-dom";

const client = createClient(CustomerService, transport);

export function CustomersPage() {
  const navigate = useNavigate();
  const [customers, setCustomers] = useState<Customer[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [name, setName] = useState("");
  const [externalId, setExternalId] = useState("");
  const [search, setSearch] = useState("");
  const [copiedId, setCopiedId] = useState<string | null>(null);

  async function load() {
    try {
      const resp = await client.listCustomers({});
      setCustomers(resp.customers);
    } catch (e) { console.error(e); }
    setLoading(false);
  }

  useEffect(() => { load(); }, []);

  async function handleCreate() {
    if (!name.trim()) return;
    await client.createCustomer({ name, externalId });
    setName(""); setExternalId(""); setShowCreate(false);
    load();
  }

  function copyId(id: string) {
    navigator.clipboard.writeText(id);
    setCopiedId(id);
    setTimeout(() => setCopiedId(null), 2000);
  }

  const filtered = customers.filter(c =>
    !search || c.name.toLowerCase().includes(search.toLowerCase()) ||
    (c.externalId && c.externalId.toLowerCase().includes(search.toLowerCase())) ||
    c.id.includes(search)
  );

  return (
    <div className="p-8 max-w-6xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Customers</h1>
          <p className="text-sm text-gray-500 mt-1">Manage billable entities. Each customer can have contracts, invoices, and usage data.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Add Customer
        </button>
      </div>

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New Customer</h3>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Name <span className="text-red-400">*</span></label>
              <input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. Acme Corp"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" autoFocus />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">External ID</label>
              <input value={externalId} onChange={e => setExternalId(e.target.value)} placeholder="Your internal ID (optional)"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
          </div>
          <div className="flex gap-2 mt-4">
            <button onClick={handleCreate} className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700">Create</button>
            <button onClick={() => setShowCreate(false)} className="px-4 py-2 text-gray-600 rounded-lg text-sm hover:bg-gray-100">Cancel</button>
          </div>
        </div>
      )}

      {/* Search */}
      <div className="relative mb-4">
        <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-gray-400" />
        <input value={search} onChange={e => setSearch(e.target.value)} placeholder="Search by name, external ID, or customer ID..."
          className="w-full pl-10 pr-4 py-2.5 bg-white border border-gray-200 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
      </div>

      {loading ? (
        <div className="text-center py-12 text-gray-400">Loading...</div>
      ) : filtered.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Users className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">{search ? "No matching customers" : "No customers yet"}</p>
          <p className="text-sm text-gray-400 mt-1">{search ? "Try a different search term." : "Create your first customer to get started."}</p>
        </div>
      ) : (
        <div className="bg-white rounded-xl border border-gray-200 overflow-hidden shadow-sm">
          <table className="w-full">
            <thead>
              <tr className="border-b border-gray-100 bg-gray-50/50">
                <th className="text-left px-4 py-3">Customer</th>
                <th className="text-left px-4 py-3">External ID</th>
                <th className="text-left px-4 py-3">Customer ID</th>
                <th className="text-left px-4 py-3">Created</th>
                <th className="w-8"></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map(c => (
                <tr key={c.id} onClick={() => navigate(`/customers/${c.id}`)} className="border-b border-gray-50 hover:bg-indigo-50/30 cursor-pointer">
                  <td className="px-4 py-3">
                    <div className="flex items-center gap-3">
                      <div className="w-8 h-8 rounded-full bg-indigo-100 flex items-center justify-center text-indigo-700 text-xs font-bold">
                        {c.name.charAt(0).toUpperCase()}
                      </div>
                      <span className="font-medium text-sm text-gray-900">{c.name}</span>
                    </div>
                  </td>
                  <td className="px-4 py-3">
                    {c.externalId ? (
                      <span className="inline-flex items-center gap-1 px-2 py-0.5 bg-gray-100 rounded text-xs font-mono text-gray-600">
                        <ExternalLink className="w-3 h-3" />{c.externalId}
                      </span>
                    ) : (
                      <span className="text-xs text-gray-300">—</span>
                    )}
                  </td>
                  <td className="px-4 py-3">
                    <button onClick={(e) => { e.stopPropagation(); copyId(c.id); }}
                      className="inline-flex items-center gap-1 px-2 py-0.5 bg-gray-50 hover:bg-gray-100 rounded text-xs font-mono text-gray-500 transition-colors">
                      {copiedId === c.id ? <Check className="w-3 h-3 text-emerald-500" /> : <Copy className="w-3 h-3" />}
                      {c.id.slice(0, 16)}…
                    </button>
                  </td>
                  <td className="px-4 py-3 text-sm text-gray-500">
                    {new Date(Number(c.createdAt)).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" })}
                  </td>
                  <td className="pr-3">
                    <ChevronRight className="w-4 h-4 text-gray-300" />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="px-4 py-3 border-t border-gray-100 bg-gray-50/30 text-xs text-gray-400">
            {filtered.length} customer{filtered.length !== 1 ? "s" : ""}
          </div>
        </div>
      )}
    </div>
  );
}
