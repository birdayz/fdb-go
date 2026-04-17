import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { ContractService } from "@/gen/metrognome/v1/contract_pb";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { PlanService } from "@/gen/metrognome/v1/plan_pb";
import { ScrollText, Plus, Calendar, CheckCircle2, XCircle, Clock, DollarSign } from "lucide-react";

const contractClient = createClient(ContractService, transport);
const customerClient = createClient(CustomerService, transport);
const planClient = createClient(PlanService, transport);

const billingPeriodLabels: Record<number, string> = {
  0: "Unspecified",
  1: "Monthly",
  2: "Quarterly",
  3: "Annual",
};

export function ContractsPage() {
  const [contracts, setContracts] = useState<any[]>([]);
  const [customers, setCustomers] = useState<any[]>([]);
  const [plans, setPlans] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);

  // Create form
  const [customerId, setCustomerId] = useState("");
  const [planId, setPlanId] = useState("");
  const [billingPeriod, setBillingPeriod] = useState(1);
  const [startDate, setStartDate] = useState(() => new Date().toISOString().split("T")[0]);
  const [endDate, setEndDate] = useState("");
  const [commitAmount, setCommitAmount] = useState("");
  const [overageMultiplier, setOverageMultiplier] = useState("");

  async function load() {
    try {
      const [contractResp, customerResp, planResp] = await Promise.all([
        contractClient.listContracts({}),
        customerClient.listCustomers({}),
        planClient.listPlans({}),
      ]);
      setContracts(contractResp.contracts);
      setCustomers(customerResp.customers);
      setPlans(planResp.plans);
    } catch (e) { console.error(e); }
    setLoading(false);
  }

  useEffect(() => { load(); }, []);

  const customerMap = Object.fromEntries(customers.map(c => [c.id, c.name]));
  const planMap = Object.fromEntries(plans.map(p => [p.id, p.name]));

  async function handleCreate() {
    if (!customerId || !planId) return;
    const startMs = new Date(startDate).getTime();
    const endMs = endDate ? new Date(endDate).getTime() : 0;
    const commitCents = commitAmount ? Math.round(parseFloat(commitAmount) * 100) : 0;
    const overageBps = overageMultiplier ? Math.round(parseFloat(overageMultiplier) * 10000) : 0;
    await contractClient.createContract({
      customerId,
      planId,
      startAt: BigInt(startMs),
      endAt: BigInt(endMs),
      billingPeriod,
      committedAmountCents: BigInt(commitCents),
      overageMultiplierBps: BigInt(overageBps),
    });
    setShowCreate(false);
    setCustomerId(""); setPlanId(""); setEndDate("");
    load();
  }

  async function handleEnd(contractId: string) {
    await contractClient.endContract({ id: contractId, endAt: BigInt(Date.now()) });
    load();
  }

  return (
    <div className="p-8 max-w-6xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Contracts</h1>
          <p className="text-sm text-gray-500 mt-1">Customer billing agreements linking a plan, billing period, and duration.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Create Contract
        </button>
      </div>

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New Contract</h3>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Customer <span className="text-red-400">*</span></label>
              <select value={customerId} onChange={e => setCustomerId(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select customer...</option>
                {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Plan <span className="text-red-400">*</span></label>
              <select value={planId} onChange={e => setPlanId(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select plan...</option>
                {plans.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Billing Period</label>
              <select value={billingPeriod} onChange={e => setBillingPeriod(Number(e.target.value))}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value={1}>Monthly</option>
                <option value={2}>Quarterly</option>
                <option value={3}>Annual</option>
              </select>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">Start Date</label>
                <input type="date" value={startDate} onChange={e => setStartDate(e.target.value)}
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
              </div>
              <div>
                <label className="block text-sm font-medium text-gray-700 mb-1">End Date</label>
                <input type="date" value={endDate} onChange={e => setEndDate(e.target.value)} placeholder="Indefinite"
                  className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
              </div>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-4 mt-3">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Prepaid Commit ($/period)</label>
              <input type="number" step="0.01" value={commitAmount} onChange={e => setCommitAmount(e.target.value)} placeholder="0 = no commit"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Overage Multiplier</label>
              <input type="number" step="0.1" value={overageMultiplier} onChange={e => setOverageMultiplier(e.target.value)} placeholder="1.0 = standard, 1.5 = 150%"
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
      ) : contracts.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <ScrollText className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No contracts yet</p>
          <p className="text-sm text-gray-400 mt-1">Create a contract to link a customer with a plan.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {contracts.map((ct: any) => {
            const isActive = ct.active;
            const startMs = Number(ct.startAt);
            const endMs = Number(ct.endAt);
            return (
              <div key={ct.id} className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
                <div className="flex items-start justify-between">
                  <div className="flex items-center gap-3">
                    <div className={`w-10 h-10 rounded-lg flex items-center justify-center ${isActive ? "bg-emerald-50" : "bg-gray-50"}`}>
                      <ScrollText className={`w-5 h-5 ${isActive ? "text-emerald-600" : "text-gray-400"}`} />
                    </div>
                    <div>
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-semibold text-gray-900">
                          {customerMap[ct.customerId] || ct.customerId.slice(0, 12)}
                        </span>
                        <span className="text-gray-300">-</span>
                        <span className="text-sm text-gray-600">
                          {planMap[ct.planId] || ct.planId.slice(0, 12)}
                        </span>
                      </div>
                      <div className="text-xs text-gray-400 font-mono mt-0.5">{ct.id}</div>
                    </div>
                  </div>
                  <div className="flex items-center gap-3">
                    {isActive ? (
                      <span className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-semibold bg-emerald-100 text-emerald-800 border border-emerald-200">
                        <CheckCircle2 className="w-3 h-3" /> Active
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-semibold bg-gray-100 text-gray-600 border border-gray-200">
                        <XCircle className="w-3 h-3" /> Ended
                      </span>
                    )}
                    {isActive && (
                      <button onClick={() => handleEnd(ct.id)} className="px-3 py-1.5 text-xs font-medium text-red-600 hover:bg-red-50 rounded-lg border border-red-200">
                        End
                      </button>
                    )}
                  </div>
                </div>

                <div className="flex gap-6 mt-3 text-xs text-gray-500">
                  <span className="flex items-center gap-1">
                    <Calendar className="w-3 h-3" />
                    {new Date(startMs).toLocaleDateString()} — {endMs > 0 ? new Date(endMs).toLocaleDateString() : "Indefinite"}
                  </span>
                  <span className="flex items-center gap-1">
                    <Clock className="w-3 h-3" />
                    {billingPeriodLabels[ct.billingPeriod] || "Monthly"}
                  </span>
                  {Number(ct.committedAmountCents) > 0 && (
                    <span className="flex items-center gap-1 px-2 py-0.5 bg-indigo-50 text-indigo-700 rounded-full font-medium">
                      <DollarSign className="w-3 h-3" />
                      ${(Number(ct.committedAmountCents) / 100).toLocaleString()}/period commit
                      {Number(ct.overageMultiplierBps) > 0 && Number(ct.overageMultiplierBps) !== 10000 && (
                        <span className="text-indigo-400 ml-1">({(Number(ct.overageMultiplierBps) / 10000).toFixed(1)}x overage)</span>
                      )}
                    </span>
                  )}
                  <span className="ml-auto">Created {new Date(Number(ct.createdAt)).toLocaleDateString()}</span>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
