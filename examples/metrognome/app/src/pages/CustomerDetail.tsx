import { useState, useEffect } from "react";
import { useParams, Link } from "react-router-dom";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { EventService } from "@/gen/metrognome/v1/event_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import { ContractService } from "@/gen/metrognome/v1/contract_pb";
import { CreditService } from "@/gen/metrognome/v1/credit_pb";
import { InvoiceService } from "@/gen/metrognome/v1/invoice_pb";
import { AlertService } from "@/gen/metrognome/v1/alert_pb";
import {
  ArrowLeft, Activity, Gauge, ScrollText, CreditCard, FileText,
  Bell, Zap, Clock, CheckCircle2, AlertTriangle, TrendingUp,
} from "lucide-react";

const customerClient = createClient(CustomerService, transport);
const eventClient = createClient(EventService, transport);
const meterClient = createClient(MeterService, transport);
const contractClient = createClient(ContractService, transport);
const creditClient = createClient(CreditService, transport);
const invoiceClient = createClient(InvoiceService, transport);
const alertClient = createClient(AlertService, transport);

function formatCents(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

function formatNumber(n: number): string {
  if (n >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`;
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toString();
}

export function CustomerDetailPage() {
  const { id } = useParams<{ id: string }>();
  const [customer, setCustomer] = useState<any>(null);
  const [meters, setMeters] = useState<any[]>([]);
  const [usage, setUsage] = useState<Record<string, { total: number; label: string }>>({});
  const [contracts, setContracts] = useState<any[]>([]);
  const [credits, setCredits] = useState<any[]>([]);
  const [invoices, setInvoices] = useState<any[]>([]);
  const [alerts, setAlerts] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [usageLoadTime, setUsageLoadTime] = useState<number | null>(null);

  useEffect(() => {
    if (!id) return;
    loadAll();
  }, [id]);

  async function loadAll() {
    setLoading(true);
    const [custResp, meterResp, contractResp, creditResp, invoiceResp, alertResp] = await Promise.all([
      customerClient.getCustomer({ id: id! }).catch(() => null),
      meterClient.listMeters({}).catch(() => ({ meters: [] })),
      contractClient.listContracts({ customerId: id! }).catch(() => ({ contracts: [] })),
      creditClient.listCredits({ customerId: id! }).catch(() => ({ credits: [] })),
      invoiceClient.listInvoices({ customerId: id! }).catch(() => ({ invoices: [] })),
      alertClient.listAlerts({ customerId: id! }).catch(() => ({ alerts: [] })),
    ]);

    if (custResp?.customer) setCustomer(custResp.customer);
    const ms = meterResp.meters || [];
    setMeters(ms);
    setContracts(contractResp.contracts || []);
    setCredits(creditResp.credits || []);
    setInvoices(invoiceResp.invoices || []);
    setAlerts(alertResp.alerts || []);

    // Load usage for each meter — O(1) per meter via FDB atomic indexes
    const now = Date.now();
    const thirtyDaysAgo = now - 30 * 86400000;
    const t0 = performance.now();
    const usageMap: Record<string, { total: number; label: string }> = {};
    await Promise.all(ms.map(async (m: any) => {
      try {
        const resp = await eventClient.getUsage({
          customerId: id!,
          meterSlug: m.slug,
          startMs: BigInt(thirtyDaysAgo),
          endMs: BigInt(now),
        });
        usageMap[m.slug] = { total: Number(resp.totalValue), label: m.name };
      } catch { /* no usage */ }
    }));
    setUsageLoadTime(Math.round(performance.now() - t0));
    setUsage(usageMap);
    setLoading(false);
  }

  if (loading) {
    return (
      <div className="p-8 flex items-center justify-center h-64">
        <div className="flex flex-col items-center gap-3">
          <div className="w-8 h-8 border-2 border-indigo-600 border-t-transparent rounded-full animate-spin" />
          <span className="text-sm text-gray-400">Loading customer...</span>
        </div>
      </div>
    );
  }

  if (!customer) {
    return (
      <div className="p-8">
        <Link to="/customers" className="flex items-center gap-1 text-sm text-indigo-600 hover:text-indigo-700 mb-4">
          <ArrowLeft className="w-4 h-4" /> Back to Customers
        </Link>
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <p className="text-gray-500 font-medium">Customer not found</p>
        </div>
      </div>
    );
  }

  const activeContracts = contracts.filter((c: any) => c.active);
  const totalCreditsRemaining = credits.reduce((sum: number, c: any) => sum + Number(c.remainingCents), 0);
  const totalCreditsOriginal = credits.reduce((sum: number, c: any) => sum + Number(c.amountCents), 0);
  const triggeredAlerts = alerts.filter((a: any) => a.triggered);

  return (
    <div className="p-8 max-w-6xl">
      {/* Header */}
      <Link to="/customers" className="flex items-center gap-1 text-sm text-indigo-600 hover:text-indigo-700 mb-4">
        <ArrowLeft className="w-4 h-4" /> Back to Customers
      </Link>

      <div className="flex items-center gap-4 mb-6">
        <div className="w-14 h-14 rounded-xl bg-indigo-100 flex items-center justify-center text-indigo-700 text-xl font-bold">
          {customer.name.charAt(0).toUpperCase()}
        </div>
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{customer.name}</h1>
          <div className="flex items-center gap-3 mt-1 text-xs text-gray-400">
            <span className="font-mono">{customer.id}</span>
            {customer.externalId && <span className="px-2 py-0.5 bg-gray-100 rounded text-gray-600">{customer.externalId}</span>}
            <span>Created {new Date(Number(customer.createdAt)).toLocaleDateString()}</span>
          </div>
        </div>
      </div>

      {/* Quick Stats */}
      <div className="grid grid-cols-4 gap-4 mb-6">
        <StatCard icon={ScrollText} label="Active Contracts" value={activeContracts.length} color="bg-blue-50 text-blue-600" />
        <StatCard icon={CreditCard} label="Credit Balance" value={formatCents(totalCreditsRemaining)} color="bg-emerald-50 text-emerald-600" />
        <StatCard icon={FileText} label="Invoices" value={invoices.length} color="bg-purple-50 text-purple-600" />
        <StatCard icon={Bell} label="Triggered Alerts" value={triggeredAlerts.length} color={triggeredAlerts.length > 0 ? "bg-red-50 text-red-600" : "bg-gray-50 text-gray-400"} />
      </div>

      {/* Usage — O(1) reads from FDB atomic indexes */}
      <div className="bg-white rounded-xl border border-gray-200 p-5 mb-6 shadow-sm">
        <div className="flex items-center justify-between mb-4">
          <div className="flex items-center gap-2">
            <Activity className="w-5 h-5 text-indigo-600" />
            <h2 className="font-semibold text-gray-900">Usage (Last 30 Days)</h2>
          </div>
          {usageLoadTime !== null && (
            <div className="flex items-center gap-1 px-2 py-1 bg-emerald-50 rounded-full">
              <Zap className="w-3 h-3 text-emerald-600" />
              <span className="text-[11px] font-semibold text-emerald-700">
                {meters.length} metrics queried in {usageLoadTime}ms — O(1) per metric via FDB atomic indexes
              </span>
            </div>
          )}
        </div>
        {Object.keys(usage).length === 0 ? (
          <p className="text-sm text-gray-400 py-4 text-center">No usage data in the last 30 days.</p>
        ) : (
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-3">
            {Object.entries(usage).map(([slug, { total, label }]) => (
              <div key={slug} className="p-4 bg-gray-50 rounded-lg border border-gray-100">
                <div className="flex items-center gap-2 mb-1">
                  <Gauge className="w-4 h-4 text-gray-400" />
                  <span className="text-xs font-medium text-gray-500 uppercase tracking-wider">{label}</span>
                </div>
                <div className="text-2xl font-bold text-gray-900">{formatNumber(total)}</div>
                <div className="text-xs text-gray-400 font-mono mt-0.5">{slug}</div>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* Two-column: Contracts + Credits */}
      <div className="grid grid-cols-2 gap-6 mb-6">
        {/* Active Contracts */}
        <div className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
          <div className="flex items-center gap-2 mb-4">
            <ScrollText className="w-5 h-5 text-blue-600" />
            <h2 className="font-semibold text-gray-900">Contracts</h2>
          </div>
          {contracts.length === 0 ? (
            <p className="text-sm text-gray-400 py-4 text-center">No contracts.</p>
          ) : (
            <div className="space-y-2">
              {contracts.map((ct: any) => (
                <div key={ct.id} className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
                  <div>
                    <div className="text-sm font-medium text-gray-900">
                      {ct.planId}
                    </div>
                    <div className="text-xs text-gray-400">
                      {new Date(Number(ct.startAt)).toLocaleDateString()} — {Number(ct.endAt) > 0 ? new Date(Number(ct.endAt)).toLocaleDateString() : "Indefinite"}
                    </div>
                  </div>
                  {ct.active ? (
                    <span className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs font-semibold bg-emerald-100 text-emerald-700">
                      <CheckCircle2 className="w-3 h-3" /> Active
                    </span>
                  ) : (
                    <span className="text-xs text-gray-400">Ended</span>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>

        {/* Credit Balances */}
        <div className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
          <div className="flex items-center gap-2 mb-4">
            <CreditCard className="w-5 h-5 text-emerald-600" />
            <h2 className="font-semibold text-gray-900">Credits</h2>
            {totalCreditsOriginal > 0 && (
              <span className="ml-auto text-sm text-gray-400">
                {formatCents(totalCreditsRemaining)} / {formatCents(totalCreditsOriginal)} remaining
              </span>
            )}
          </div>
          {credits.length === 0 ? (
            <p className="text-sm text-gray-400 py-4 text-center">No credits granted.</p>
          ) : (
            <div className="space-y-2">
              {credits.map((cr: any) => {
                const remaining = Number(cr.remainingCents);
                const total = Number(cr.amountCents);
                const pct = total > 0 ? Math.round((remaining / total) * 100) : 0;
                return (
                  <div key={cr.id} className="p-3 bg-gray-50 rounded-lg">
                    <div className="flex justify-between mb-1.5">
                      <span className="text-sm font-medium text-gray-900">{formatCents(remaining)}</span>
                      <span className="text-xs text-gray-400">of {formatCents(total)} • Priority {cr.priority}</span>
                    </div>
                    <div className="w-full bg-gray-200 rounded-full h-1.5">
                      <div className={`h-1.5 rounded-full ${pct > 50 ? "bg-emerald-500" : pct > 20 ? "bg-amber-500" : "bg-red-500"}`}
                        style={{ width: `${pct}%` }} />
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>

      {/* Recent Invoices */}
      <div className="bg-white rounded-xl border border-gray-200 p-5 mb-6 shadow-sm">
        <div className="flex items-center gap-2 mb-4">
          <FileText className="w-5 h-5 text-purple-600" />
          <h2 className="font-semibold text-gray-900">Invoice History</h2>
        </div>
        {invoices.length === 0 ? (
          <p className="text-sm text-gray-400 py-4 text-center">No invoices generated yet.</p>
        ) : (
          <div className="space-y-2">
            {invoices.slice(0, 5).map((inv: any) => {
              const statusColors: Record<number, string> = {
                1: "bg-yellow-100 text-yellow-800",
                2: "bg-blue-100 text-blue-800",
                3: "bg-emerald-100 text-emerald-800",
                4: "bg-red-100 text-red-800",
              };
              const statusLabels: Record<number, string> = { 1: "Draft", 2: "Issued", 3: "Paid", 4: "Void" };
              return (
                <div key={inv.id} className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
                  <div>
                    <span className="text-sm font-medium text-gray-900">
                      {new Date(Number(inv.periodStart)).toLocaleDateString("en-US", { month: "short", day: "numeric" })} — {new Date(Number(inv.periodEnd)).toLocaleDateString("en-US", { month: "short", day: "numeric", year: "numeric" })}
                    </span>
                    <div className="text-xs text-gray-400 font-mono">{inv.id}</div>
                  </div>
                  <div className="flex items-center gap-3">
                    <span className={`px-2 py-0.5 rounded-full text-xs font-semibold ${statusColors[inv.status] || "bg-gray-100 text-gray-600"}`}>
                      {statusLabels[inv.status] || "Unknown"}
                    </span>
                    <span className="text-sm font-bold text-gray-900">{formatCents(Number(inv.totalCents))}</span>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* Alerts */}
      {alerts.length > 0 && (
        <div className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
          <div className="flex items-center gap-2 mb-4">
            <Bell className="w-5 h-5 text-amber-600" />
            <h2 className="font-semibold text-gray-900">Alerts</h2>
          </div>
          <div className="space-y-2">
            {alerts.map((al: any) => (
              <div key={al.id} className="flex items-center justify-between p-3 bg-gray-50 rounded-lg">
                <div className="flex items-center gap-2">
                  {al.triggered ? <AlertTriangle className="w-4 h-4 text-red-500" /> : <Bell className="w-4 h-4 text-emerald-500" />}
                  <span className="text-sm font-medium text-gray-900">{al.meterSlug}</span>
                  <span className="text-xs text-gray-400">threshold: {Number(al.threshold).toLocaleString()}</span>
                </div>
                {al.triggered ? (
                  <span className="px-2 py-0.5 rounded-full text-xs font-semibold bg-red-100 text-red-700">Triggered</span>
                ) : (
                  <span className="px-2 py-0.5 rounded-full text-xs font-semibold bg-emerald-100 text-emerald-700">Active</span>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

function StatCard({ icon: Icon, label, value, color }: { icon: typeof Activity; label: string; value: string | number; color: string }) {
  return (
    <div className="bg-white rounded-xl border border-gray-200 p-4 shadow-sm">
      <div className={`w-9 h-9 rounded-lg ${color} flex items-center justify-center mb-2`}>
        <Icon className="w-4.5 h-4.5" />
      </div>
      <div className="text-xl font-bold text-gray-900">{value}</div>
      <div className="text-xs text-gray-500 mt-0.5">{label}</div>
    </div>
  );
}
