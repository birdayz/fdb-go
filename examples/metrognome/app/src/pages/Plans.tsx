import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { PlanService } from "@/gen/metrognome/v1/plan_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import type { Plan, Charge } from "@/gen/metrognome/v1/plan_pb";
import { Layers, Plus, ChevronDown, ChevronUp, DollarSign, Tag, Hash } from "lucide-react";

const planClient = createClient(PlanService, transport);
const meterClient = createClient(MeterService, transport);

function formatCents(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

const pricingLabels: Record<string, { label: string; color: string }> = {
  flat: { label: "Flat", color: "bg-gray-100 text-gray-700" },
  perUnit: { label: "Per Unit", color: "bg-blue-100 text-blue-700" },
  tiered: { label: "Tiered", color: "bg-purple-100 text-purple-700" },
  volume: { label: "Volume", color: "bg-amber-100 text-amber-700" },
  package: { label: "Package", color: "bg-emerald-100 text-emerald-700" },
  bps: { label: "BPS", color: "bg-rose-100 text-rose-700" },
};

function getPricingType(charge: any): string {
  const p = charge.pricing;
  if (!p) return "flat";
  if (p.flat) return "flat";
  if (p.perUnit) return "perUnit";
  if (p.tiered) return "tiered";
  if (p.volume) return "volume";
  if (p.package) return "package";
  if (p.bps) return "bps";
  return "flat";
}

function getPricingSummary(charge: any): string {
  const p = charge.pricing;
  if (!p) return "—";
  if (p.flat) return formatCents(Number(p.flat.amountCents));
  if (p.perUnit) return `${formatCents(Number(p.perUnit.unitPriceCents))}/unit`;
  if (p.tiered) return `${p.tiered.tiers.length} tier${p.tiered.tiers.length !== 1 ? "s" : ""}`;
  if (p.volume) return `${p.volume.tiers.length} tier${p.volume.tiers.length !== 1 ? "s" : ""}`;
  if (p.package) return `${formatCents(Number(p.package.packagePriceCents))} / ${p.package.packageSize} units`;
  if (p.bps) return `${Number(p.bps.basisPoints)} bps`;
  return "—";
}

export function PlansPage() {
  const [plans, setPlans] = useState<Plan[]>([]);
  const [meters, setMeters] = useState<any[]>([]);
  const [charges, setCharges] = useState<Record<string, any[]>>({});
  const [loading, setLoading] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [showAddCharge, setShowAddCharge] = useState<string | null>(null);

  // Create plan form
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  // Add charge form
  const [chargeMeterSlug, setChargeMeterSlug] = useState("");
  const [chargePricingType, setChargePricingType] = useState("per_unit");
  const [chargeAmountCents, setChargeAmountCents] = useState("");
  const [chargeTiers, setChargeTiers] = useState([{ upTo: "", priceCents: "" }]);
  const [chargePackageSize, setChargePackageSize] = useState("");
  const [chargeBasisPoints, setChargeBasisPoints] = useState("");

  async function load() {
    try {
      const [planResp, meterResp] = await Promise.all([
        planClient.listPlans({}),
        meterClient.listMeters({}),
      ]);
      setPlans(planResp.plans);
      setMeters(meterResp.meters);
    } catch (e) { console.error(e); }
    setLoading(false);
  }

  useEffect(() => { load(); }, []);

  async function loadCharges(planId: string) {
    try {
      const resp = await planClient.listCharges({ planId });
      setCharges(prev => ({ ...prev, [planId]: resp.charges }));
    } catch (e) { console.error(e); }
  }

  function toggleExpand(planId: string) {
    if (expanded === planId) {
      setExpanded(null);
    } else {
      setExpanded(planId);
      if (!charges[planId]) loadCharges(planId);
    }
  }

  async function handleCreate() {
    if (!name.trim()) return;
    await planClient.createPlan({ name, description });
    setName(""); setDescription(""); setShowCreate(false);
    load();
  }

  async function handleAddCharge(planId: string) {
    if (!chargeMeterSlug) return;
    let pricing: any = {};
    switch (chargePricingType) {
      case "flat":
        pricing = { flat: { amountCents: BigInt(Math.round(parseFloat(chargeAmountCents) * 100) || 0) } };
        break;
      case "per_unit":
        pricing = { perUnit: { unitPriceCents: BigInt(Math.round(parseFloat(chargeAmountCents) * 100) || 0) } };
        break;
      case "tiered":
        pricing = { tiered: { tiers: chargeTiers.map(t => ({ upTo: BigInt(parseInt(t.upTo) || 0), priceCents: BigInt(Math.round(parseFloat(t.priceCents) * 100) || 0) })) } };
        break;
      case "volume":
        pricing = { volume: { tiers: chargeTiers.map(t => ({ upTo: BigInt(parseInt(t.upTo) || 0), priceCents: BigInt(Math.round(parseFloat(t.priceCents) * 100) || 0) })) } };
        break;
      case "package":
        pricing = { package: { packageSize: BigInt(parseInt(chargePackageSize) || 1), packagePriceCents: BigInt(Math.round(parseFloat(chargeAmountCents) * 100) || 0) } };
        break;
      case "bps":
        pricing = { bps: { basisPoints: BigInt(parseInt(chargeBasisPoints) || 0) } };
        break;
    }
    await planClient.addCharge({ planId, meterSlug: chargeMeterSlug, pricing });
    setShowAddCharge(null);
    setChargeMeterSlug(""); setChargeAmountCents(""); setChargePricingType("per_unit");
    setChargeTiers([{ upTo: "", priceCents: "" }]); setChargePackageSize(""); setChargeBasisPoints("");
    loadCharges(planId);
  }

  return (
    <div className="p-8 max-w-6xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Plans</h1>
          <p className="text-sm text-gray-500 mt-1">Pricing plans bundle charges for billable metrics. Attach plans to contracts.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Create Plan
        </button>
      </div>

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New Plan</h3>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Name <span className="text-red-400">*</span></label>
              <input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. Starter Plan"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Description</label>
              <input value={description} onChange={e => setDescription(e.target.value)} placeholder="Plan description"
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
      ) : plans.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Layers className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No plans yet</p>
          <p className="text-sm text-gray-400 mt-1">Create a plan and add charges to start pricing usage.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {plans.map(plan => {
            const isExpanded = expanded === plan.id;
            const planCharges = charges[plan.id] || [];
            return (
              <div key={plan.id} className="bg-white rounded-xl border border-gray-200 overflow-hidden shadow-sm">
                <button onClick={() => toggleExpand(plan.id)}
                  className="w-full flex items-center justify-between px-5 py-4 hover:bg-gray-50/50 transition-colors">
                  <div className="flex items-center gap-3">
                    <div className="w-10 h-10 rounded-lg bg-indigo-50 flex items-center justify-center">
                      <Layers className="w-5 h-5 text-indigo-600" />
                    </div>
                    <div className="text-left">
                      <div className="text-sm font-semibold text-gray-900">{plan.name}</div>
                      {plan.description && <div className="text-xs text-gray-400 mt-0.5">{plan.description}</div>}
                    </div>
                  </div>
                  <div className="flex items-center gap-3">
                    <span className="text-xs text-gray-400 font-mono">{plan.id.slice(0, 12)}...</span>
                    <span className="text-xs text-gray-400">{new Date(Number(plan.createdAt)).toLocaleDateString()}</span>
                    {isExpanded ? <ChevronUp className="w-4 h-4 text-gray-400" /> : <ChevronDown className="w-4 h-4 text-gray-400" />}
                  </div>
                </button>

                {isExpanded && (
                  <div className="border-t border-gray-100 px-5 py-4">
                    <div className="flex items-center justify-between mb-3">
                      <h4 className="text-sm font-semibold text-gray-700">Charges</h4>
                      <button onClick={() => setShowAddCharge(showAddCharge === plan.id ? null : plan.id)}
                        className="flex items-center gap-1 px-3 py-1.5 text-xs font-medium text-indigo-600 hover:bg-indigo-50 rounded-lg">
                        <Plus className="w-3 h-3" /> Add Charge
                      </button>
                    </div>

                    {showAddCharge === plan.id && (
                      <div className="bg-gray-50 rounded-lg p-4 mb-4 border border-gray-100">
                        <div className="grid grid-cols-2 gap-3">
                          <div>
                            <label className="block text-xs font-medium text-gray-600 mb-1">Meter</label>
                            <select value={chargeMeterSlug} onChange={e => setChargeMeterSlug(e.target.value)}
                              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none bg-white">
                              <option value="">Select meter...</option>
                              {meters.map(m => <option key={m.slug} value={m.slug}>{m.name} ({m.slug})</option>)}
                            </select>
                          </div>
                          <div>
                            <label className="block text-xs font-medium text-gray-600 mb-1">Pricing Model</label>
                            <select value={chargePricingType} onChange={e => setChargePricingType(e.target.value)}
                              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none bg-white">
                              <option value="flat">Flat Fee</option>
                              <option value="per_unit">Per Unit</option>
                              <option value="tiered">Tiered</option>
                              <option value="volume">Volume</option>
                              <option value="package">Package</option>
                              <option value="bps">Basis Points</option>
                            </select>
                          </div>
                        </div>

                        {(chargePricingType === "flat" || chargePricingType === "per_unit" || chargePricingType === "package") && (
                          <div className="grid grid-cols-2 gap-3 mt-3">
                            <div>
                              <label className="block text-xs font-medium text-gray-600 mb-1">
                                {chargePricingType === "flat" ? "Amount ($)" : chargePricingType === "per_unit" ? "Price per unit ($)" : "Package price ($)"}
                              </label>
                              <input type="number" step="0.01" value={chargeAmountCents} onChange={e => setChargeAmountCents(e.target.value)} placeholder="0.00"
                                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
                            </div>
                            {chargePricingType === "package" && (
                              <div>
                                <label className="block text-xs font-medium text-gray-600 mb-1">Package size (units)</label>
                                <input type="number" value={chargePackageSize} onChange={e => setChargePackageSize(e.target.value)} placeholder="100"
                                  className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
                              </div>
                            )}
                          </div>
                        )}

                        {chargePricingType === "bps" && (
                          <div className="mt-3">
                            <label className="block text-xs font-medium text-gray-600 mb-1">Basis points (e.g. 25 = 0.25%)</label>
                            <input type="number" value={chargeBasisPoints} onChange={e => setChargeBasisPoints(e.target.value)} placeholder="25"
                              className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none max-w-xs" />
                          </div>
                        )}

                        {(chargePricingType === "tiered" || chargePricingType === "volume") && (
                          <div className="mt-3">
                            <label className="block text-xs font-medium text-gray-600 mb-2">Tiers</label>
                            {chargeTiers.map((tier, i) => (
                              <div key={i} className="flex gap-2 mb-2 items-center">
                                <input type="number" value={tier.upTo} onChange={e => { const t = [...chargeTiers]; t[i].upTo = e.target.value; setChargeTiers(t); }}
                                  placeholder={i === chargeTiers.length - 1 ? "Infinity (0)" : "Up to"} className="flex-1 px-3 py-1.5 border border-gray-300 rounded-lg text-xs focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
                                <input type="number" step="0.01" value={tier.priceCents} onChange={e => { const t = [...chargeTiers]; t[i].priceCents = e.target.value; setChargeTiers(t); }}
                                  placeholder="$/unit" className="flex-1 px-3 py-1.5 border border-gray-300 rounded-lg text-xs focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
                                {chargeTiers.length > 1 && (
                                  <button onClick={() => setChargeTiers(chargeTiers.filter((_, j) => j !== i))} className="text-xs text-red-400 hover:text-red-600">Remove</button>
                                )}
                              </div>
                            ))}
                            <button onClick={() => setChargeTiers([...chargeTiers, { upTo: "", priceCents: "" }])} className="text-xs text-indigo-600 hover:text-indigo-700 font-medium">+ Add tier</button>
                          </div>
                        )}

                        <div className="flex gap-2 mt-3">
                          <button onClick={() => handleAddCharge(plan.id)} className="px-3 py-1.5 bg-indigo-600 text-white rounded-lg text-xs font-medium hover:bg-indigo-700">Add Charge</button>
                          <button onClick={() => setShowAddCharge(null)} className="px-3 py-1.5 text-gray-600 rounded-lg text-xs hover:bg-gray-100">Cancel</button>
                        </div>
                      </div>
                    )}

                    {planCharges.length === 0 ? (
                      <div className="text-center py-6 text-gray-400 text-sm">
                        No charges yet. Add a charge to price this plan.
                      </div>
                    ) : (
                      <div className="space-y-2">
                        {planCharges.map((ch: any) => {
                          const pt = getPricingType(ch);
                          const pl = pricingLabels[pt] || { label: "?", color: "bg-gray-100 text-gray-600" };
                          return (
                            <div key={ch.id} className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
                              <div className="flex items-center gap-3">
                                <div className="w-8 h-8 rounded-md bg-blue-50 flex items-center justify-center">
                                  <Tag className="w-4 h-4 text-blue-600" />
                                </div>
                                <div>
                                  <div className="text-sm font-medium text-gray-900">{ch.meterSlug}</div>
                                  <div className="text-xs text-gray-400">{getPricingSummary(ch)}</div>
                                </div>
                              </div>
                              <span className={`px-2.5 py-1 rounded-full text-xs font-semibold ${pl.color}`}>
                                {pl.label}
                              </span>
                            </div>
                          );
                        })}
                      </div>
                    )}
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
