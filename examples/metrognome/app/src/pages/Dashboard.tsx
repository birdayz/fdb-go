import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import { EventService } from "@/gen/metrognome/v1/event_pb";
import { InvoiceService } from "@/gen/metrognome/v1/invoice_pb";
import { ContractService } from "@/gen/metrognome/v1/contract_pb";
import { CreditService } from "@/gen/metrognome/v1/credit_pb";
import {
  Users, Gauge, FileText, ScrollText, CreditCard, Zap,
  TrendingUp, ArrowUpRight, DollarSign,
} from "lucide-react";
import { Link } from "react-router-dom";

const customerClient = createClient(CustomerService, transport);
const meterClient = createClient(MeterService, transport);
const eventClient = createClient(EventService, transport);
const invoiceClient = createClient(InvoiceService, transport);
const contractClient = createClient(ContractService, transport);
const creditClient = createClient(CreditService, transport);

interface UsageSeries {
  meterSlug: string;
  meterName: string;
  total: bigint;
  buckets: { startMs: bigint; value: bigint }[];
}

function formatCents(cents: number): string {
  if (cents >= 100_00) return `$${(cents / 100).toLocaleString(undefined, { minimumFractionDigits: 0, maximumFractionDigits: 0 })}`;
  return `$${(cents / 100).toFixed(2)}`;
}

export function DashboardPage() {
  const [customers, setCustomers] = useState<any[]>([]);
  const [meters, setMeters] = useState<any[]>([]);
  const [invoices, setInvoices] = useState<any[]>([]);
  const [contracts, setContracts] = useState<any[]>([]);
  const [credits, setCredits] = useState<any[]>([]);
  const [usageSeries, setUsageSeries] = useState<UsageSeries[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    async function load() {
      const [c, m, inv, ctr] = await Promise.all([
        customerClient.listCustomers({}).catch(() => ({ customers: [] })),
        meterClient.listMeters({}).catch(() => ({ meters: [] })),
        invoiceClient.listInvoices({}).catch(() => ({ invoices: [] })),
        contractClient.listContracts({}).catch(() => ({ contracts: [] })),
      ]);
      setCustomers(c.customers);
      setMeters(m.meters);
      setInvoices(inv.invoices);
      setContracts(ctr.contracts);

      // Load credits for all customers in parallel
      const creditResults = await Promise.all(
        c.customers.map((cust: any) =>
          creditClient.listCredits({ customerId: cust.id }).catch(() => ({ credits: [] }))
        )
      );
      setCredits(creditResults.flatMap(cr => cr.credits));

      // Load usage for every (meter × customer) pair in parallel — all futures fire at once
      const now = Date.now();
      const thirtyDaysAgo = now - 30 * 86400_000;
      const usageCalls = m.meters.flatMap((meter: any) =>
        c.customers.map((cust: any) => ({
          meter,
          promise: eventClient.getUsage({
            customerId: cust.id,
            meterSlug: meter.slug,
            startMs: BigInt(thirtyDaysAgo),
            endMs: BigInt(now),
            windowSize: 2, // DAY
          }).catch(() => null),
        }))
      );
      const usageResults = await Promise.all(usageCalls.map(u => u.promise));

      // Aggregate results per meter
      const seriesMap = new Map<string, UsageSeries>();
      usageCalls.forEach((call, i) => {
        const resp = usageResults[i];
        if (!resp) return;
        const slug = call.meter.slug;
        let s = seriesMap.get(slug);
        if (!s) {
          s = { meterSlug: slug, meterName: call.meter.name, total: BigInt(0), buckets: [] };
          seriesMap.set(slug, s);
        }
        s.total += resp.totalValue;
        for (const b of resp.buckets) {
          const existing = s.buckets.find(x => x.startMs === b.startMs);
          if (existing) {
            existing.value += b.value;
          } else {
            s.buckets.push({ startMs: b.startMs, value: b.value });
          }
        }
      });
      const series = [...seriesMap.values()]
        .filter(s => s.total > BigInt(0))
        .map(s => { s.buckets.sort((a, b) => Number(a.startMs - b.startMs)); return s; });
      setUsageSeries(series);
      setLoading(false);
    }
    load();
  }, []);

  const totalRevenue = invoices.reduce((sum: number, inv: any) => sum + Number(inv.subtotalCents || 0), 0);
  const totalCreditsApplied = invoices.reduce((sum: number, inv: any) => sum + Number(inv.creditsAppliedCents || 0), 0);
  const totalCollected = invoices.reduce((sum: number, inv: any) => sum + Number(inv.totalCents || 0), 0);

  return (
    <div className="p-8 max-w-7xl">
      <div className="mb-8">
        <h1 className="text-2xl font-bold text-gray-900">Dashboard</h1>
        <p className="text-sm text-gray-500 mt-1">Usage-based billing overview powered by FDB Record Layer.</p>
      </div>

      {/* Top stats */}
      <div className="grid grid-cols-2 md:grid-cols-3 lg:grid-cols-6 gap-3 mb-8">
        <MiniStat icon={Users} label="Customers" value={loading ? "-" : customers.length} to="/customers" />
        <MiniStat icon={Gauge} label="Meters" value={loading ? "-" : meters.length} to="/meters" />
        <MiniStat icon={ScrollText} label="Contracts" value={loading ? "-" : contracts.length} to="/contracts" />
        <MiniStat icon={FileText} label="Invoices" value={loading ? "-" : invoices.length} to="/invoices" />
        <MiniStat icon={CreditCard} label="Credits" value={loading ? "-" : credits.length} to="/credits" />
        <MiniStat icon={Zap} label="Meters Active" value={loading ? "-" : usageSeries.length} to="/events" />
      </div>

      {/* Revenue row */}
      {!loading && invoices.length > 0 && (
        <div className="grid grid-cols-1 md:grid-cols-3 gap-4 mb-8">
          <div className="bg-white rounded-xl border border-gray-200 p-5">
            <div className="flex items-center gap-2 text-gray-500 text-xs font-medium uppercase tracking-wider mb-2">
              <DollarSign className="w-3.5 h-3.5" /> Gross Revenue
            </div>
            <div className="text-2xl font-bold text-gray-900">{formatCents(totalRevenue)}</div>
            <div className="text-xs text-gray-400 mt-1">{invoices.length} invoices generated</div>
          </div>
          <div className="bg-white rounded-xl border border-gray-200 p-5">
            <div className="flex items-center gap-2 text-gray-500 text-xs font-medium uppercase tracking-wider mb-2">
              <CreditCard className="w-3.5 h-3.5" /> Credits Applied
            </div>
            <div className="text-2xl font-bold text-emerald-600">-{formatCents(totalCreditsApplied)}</div>
            <div className="text-xs text-gray-400 mt-1">Prepaid credits consumed</div>
          </div>
          <div className="bg-white rounded-xl border border-gray-200 p-5">
            <div className="flex items-center gap-2 text-gray-500 text-xs font-medium uppercase tracking-wider mb-2">
              <TrendingUp className="w-3.5 h-3.5" /> Net Collected
            </div>
            <div className="text-2xl font-bold text-indigo-600">{formatCents(totalCollected)}</div>
            <div className="text-xs text-gray-400 mt-1">After credit drawdown</div>
          </div>
        </div>
      )}

      {/* Usage charts */}
      {!loading && usageSeries.length > 0 && (
        <div className="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-8">
          {usageSeries.map(s => (
            <UsageChart key={s.meterSlug} series={s} />
          ))}
        </div>
      )}

      {/* Recent invoices */}
      {!loading && invoices.length > 0 && (
        <div className="bg-white rounded-xl border border-gray-200 overflow-hidden">
          <div className="px-5 py-4 border-b border-gray-100 flex items-center justify-between">
            <h2 className="font-semibold text-gray-900 text-sm">Recent Invoices</h2>
            <Link to="/invoices" className="text-xs text-indigo-600 hover:text-indigo-800 flex items-center gap-1">
              View all <ArrowUpRight className="w-3 h-3" />
            </Link>
          </div>
          <table className="w-full text-sm">
            <thead>
              <tr className="bg-gray-50">
                <th className="text-left px-5 py-2 text-xs font-medium text-gray-500 uppercase">Customer</th>
                <th className="text-right px-5 py-2 text-xs font-medium text-gray-500 uppercase">Subtotal</th>
                <th className="text-right px-5 py-2 text-xs font-medium text-gray-500 uppercase">Credits</th>
                <th className="text-right px-5 py-2 text-xs font-medium text-gray-500 uppercase">Total</th>
                <th className="text-left px-5 py-2 text-xs font-medium text-gray-500 uppercase">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-50">
              {invoices.slice(0, 5).map((inv: any, i: number) => (
                <tr key={i} className="hover:bg-gray-50/50">
                  <td className="px-5 py-3 text-gray-700 font-medium">{inv.customerId}</td>
                  <td className="px-5 py-3 text-right text-gray-900 font-mono">{formatCents(Number(inv.subtotalCents))}</td>
                  <td className="px-5 py-3 text-right text-emerald-600 font-mono">
                    {Number(inv.creditsAppliedCents) > 0 ? `-${formatCents(Number(inv.creditsAppliedCents))}` : "-"}
                  </td>
                  <td className="px-5 py-3 text-right text-gray-900 font-mono font-bold">{formatCents(Number(inv.totalCents))}</td>
                  <td className="px-5 py-3">
                    <span className={`inline-flex px-2 py-0.5 rounded-full text-xs font-medium ${
                      inv.status === 1 ? "bg-amber-50 text-amber-700" :
                      inv.status === 2 ? "bg-blue-50 text-blue-700" :
                      inv.status === 3 ? "bg-emerald-50 text-emerald-700" : "bg-gray-100 text-gray-500"
                    }`}>
                      {inv.status === 1 ? "Draft" : inv.status === 2 ? "Issued" : inv.status === 3 ? "Paid" : "Unknown"}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function MiniStat({ icon: Icon, label, value, to }: { icon: typeof Users; label: string; value: string | number; to: string }) {
  return (
    <Link to={to} className="bg-white rounded-xl border border-gray-200 p-4 hover:shadow-sm transition-all group">
      <div className="flex items-center gap-2 mb-2">
        <Icon className="w-4 h-4 text-gray-400 group-hover:text-indigo-500" />
        <span className="text-xs text-gray-500">{label}</span>
      </div>
      <div className="text-xl font-bold text-gray-900">{value}</div>
    </Link>
  );
}

function UsageChart({ series }: { series: UsageSeries }) {
  const maxVal = series.buckets.reduce((m, b) => b.value > m ? b.value : m, BigInt(0));
  const maxNum = Number(maxVal) || 1;

  return (
    <div className="bg-white rounded-xl border border-gray-200 p-5">
      <div className="flex items-center justify-between mb-4">
        <div>
          <h3 className="font-semibold text-gray-900 text-sm">{series.meterName}</h3>
          <p className="text-xs text-gray-400 mt-0.5">{series.meterSlug}</p>
        </div>
        <div className="text-right">
          <div className="text-lg font-bold text-indigo-600">{Number(series.total).toLocaleString()}</div>
          <div className="text-xs text-gray-400">total</div>
        </div>
      </div>
      <div className="flex items-end gap-[2px] h-24">
        {series.buckets.map((b, i) => {
          const pct = Math.max(2, (Number(b.value) / maxNum) * 100);
          const date = new Date(Number(b.startMs));
          return (
            <div key={i} className="flex-1 group relative">
              <div
                className="w-full bg-indigo-500 rounded-t-sm hover:bg-indigo-400 transition-colors"
                style={{ height: `${pct}%` }}
                title={`${date.toLocaleDateString()}: ${Number(b.value).toLocaleString()}`}
              />
            </div>
          );
        })}
      </div>
      <div className="flex justify-between mt-1.5">
        <span className="text-[10px] text-gray-300">
          {series.buckets.length > 0 ? new Date(Number(series.buckets[0].startMs)).toLocaleDateString(undefined, { month: "short", day: "numeric" }) : ""}
        </span>
        <span className="text-[10px] text-gray-300">
          {series.buckets.length > 0 ? new Date(Number(series.buckets[series.buckets.length - 1].startMs)).toLocaleDateString(undefined, { month: "short", day: "numeric" }) : ""}
        </span>
      </div>
    </div>
  );
}
