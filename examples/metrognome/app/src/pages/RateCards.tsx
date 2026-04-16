import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { RateCardService } from "@/gen/metrognome/v1/ratecard_pb";
import { DollarSign, Plus, CreditCard } from "lucide-react";

const client = createClient(RateCardService, transport);

export function RateCardsPage() {
  const [cards, setCards] = useState<any[]>([]);
  const [showCreate, setShowCreate] = useState(false);
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [loading, setLoading] = useState(true);

  const load = async () => {
    try {
      const res = await client.listRateCards({});
      setCards(res.rateCards);
    } catch (e) { console.error(e); }
    setLoading(false);
  };
  useEffect(() => { load(); }, []);

  const create = async () => {
    if (!name.trim()) return;
    await client.createRateCard({ name, description });
    setName(""); setDescription(""); setShowCreate(false);
    load();
  };

  return (
    <div className="p-8 max-w-6xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Rate Cards</h1>
          <p className="text-sm text-gray-500 mt-1">Centralized pricing for products. Assign rate cards to contracts — changes cascade to all customers.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Create Rate Card
        </button>
      </div>

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New Rate Card</h3>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Name</label>
              <input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. Standard Pricing" className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Description</label>
              <input value={description} onChange={e => setDescription(e.target.value)} placeholder="Optional" className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
          </div>
          <div className="flex gap-2 mt-4">
            <button onClick={create} className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700">Create</button>
            <button onClick={() => setShowCreate(false)} className="px-4 py-2 text-gray-600 rounded-lg text-sm hover:bg-gray-100">Cancel</button>
          </div>
        </div>
      )}

      {loading ? (
        <div className="text-center py-12 text-gray-400">Loading...</div>
      ) : cards.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <DollarSign className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No rate cards yet</p>
          <p className="text-sm text-gray-400 mt-1">Create a rate card to define pricing for your products.</p>
        </div>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
          {cards.map(rc => (
            <div key={rc.id} className="bg-white rounded-xl border border-gray-200 p-5 hover:shadow-md transition-shadow cursor-pointer">
              <div className="flex items-start justify-between">
                <div className="w-10 h-10 rounded-lg bg-indigo-50 flex items-center justify-center mb-3">
                  <CreditCard className="w-5 h-5 text-indigo-600" />
                </div>
              </div>
              <h3 className="font-semibold text-gray-900">{rc.name}</h3>
              {rc.description && <p className="text-sm text-gray-500 mt-1">{rc.description}</p>}
              <div className="flex items-center gap-2 mt-3">
                {(rc.aliases || []).map((a: string) => (
                  <span key={a} className="px-2 py-0.5 bg-gray-100 text-gray-600 rounded text-xs">{a}</span>
                ))}
              </div>
              <p className="text-xs text-gray-400 mt-3">Created {new Date(Number(rc.createdAt)).toLocaleDateString()}</p>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
