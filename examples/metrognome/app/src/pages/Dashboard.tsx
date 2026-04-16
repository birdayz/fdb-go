import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import { PlanService } from "@/gen/metrognome/v1/plan_pb";
import { ProductService } from "@/gen/metrognome/v1/product_pb";
import { RateCardService } from "@/gen/metrognome/v1/ratecard_pb";
import { Users, Gauge, Package, DollarSign, Activity, ArrowUpRight, Layers } from "lucide-react";
import { Link } from "react-router-dom";

export function DashboardPage() {
  const [stats, setStats] = useState({ customers: 0, meters: 0, plans: 0, products: 0, rateCards: 0 });
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    async function load() {
      const [c, m, pl, pr, rc] = await Promise.all([
        createClient(CustomerService, transport).listCustomers({}).catch(() => ({ customers: [] })),
        createClient(MeterService, transport).listMeters({}).catch(() => ({ meters: [] })),
        createClient(PlanService, transport).listPlans({}).catch(() => ({ plans: [] })),
        createClient(ProductService, transport).listProducts({}).catch(() => ({ products: [] })),
        createClient(RateCardService, transport).listRateCards({}).catch(() => ({ rateCards: [] })),
      ]);
      setStats({
        customers: c.customers.length,
        meters: m.meters.length,
        plans: pl.plans.length,
        products: pr.products.length,
        rateCards: rc.rateCards.length,
      });
      setLoading(false);
    }
    load();
  }, []);

  return (
    <div className="p-8 max-w-6xl">
      <div className="mb-8">
        <h1 className="text-2xl font-bold text-gray-900">Dashboard</h1>
        <p className="text-sm text-gray-500 mt-1">Overview of your billing platform.</p>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-5 gap-4 mb-8">
        <StatCard label="Customers" value={loading ? "–" : stats.customers} icon={Users} color="bg-blue-50 text-blue-600" to="/customers" />
        <StatCard label="Metrics" value={loading ? "–" : stats.meters} icon={Gauge} color="bg-emerald-50 text-emerald-600" to="/meters" />
        <StatCard label="Products" value={loading ? "–" : stats.products} icon={Package} color="bg-purple-50 text-purple-600" to="/products" />
        <StatCard label="Rate Cards" value={loading ? "–" : stats.rateCards} icon={DollarSign} color="bg-amber-50 text-amber-600" to="/rate-cards" />
        <StatCard label="Plans" value={loading ? "–" : stats.plans} icon={Layers} color="bg-rose-50 text-rose-600" to="/plans" />
      </div>

      <div className="bg-white rounded-xl border border-gray-200 p-6">
        <div className="flex items-center gap-2 mb-4">
          <Activity className="w-5 h-5 text-indigo-600" />
          <h2 className="font-semibold text-gray-900">Quick Start</h2>
        </div>
        <div className="space-y-1">
          <Step n={1} done={stats.customers > 0} label="Create a customer" to="/customers" />
          <Step n={2} done={stats.meters > 0} label="Define a billable metric" to="/meters" />
          <Step n={3} done={stats.products > 0} label="Create a product" to="/products" />
          <Step n={4} done={stats.rateCards > 0} label="Set up a rate card with pricing" to="/rate-cards" />
          <Step n={5} done={false} label="Create a contract for your customer" to="/contracts" />
          <Step n={6} done={false} label="Start ingesting usage events" to="/events" />
        </div>
      </div>
    </div>
  );
}

function StatCard({ label, value, icon: Icon, color, to }: { label: string; value: number | string; icon: typeof Users; color: string; to: string }) {
  return (
    <Link to={to} className="bg-white rounded-xl border border-gray-200 p-5 hover:shadow-md transition-all group">
      <div className="flex items-center justify-between mb-3">
        <div className={`w-10 h-10 rounded-lg ${color} flex items-center justify-center`}>
          <Icon className="w-5 h-5" />
        </div>
        <ArrowUpRight className="w-4 h-4 text-gray-300 group-hover:text-gray-500" />
      </div>
      <div className="text-2xl font-bold text-gray-900">{value}</div>
      <div className="text-sm text-gray-500 mt-0.5">{label}</div>
    </Link>
  );
}

function Step({ n, done, label, to }: { n: number; done: boolean; label: string; to: string }) {
  return (
    <Link to={to} className="flex items-center gap-3 p-3 rounded-lg hover:bg-gray-50 group">
      <div className={`w-7 h-7 rounded-full flex items-center justify-center text-xs font-bold ${done ? "bg-emerald-100 text-emerald-700" : "bg-gray-100 text-gray-400"}`}>
        {done ? "✓" : n}
      </div>
      <span className={`text-sm ${done ? "text-gray-400 line-through" : "text-gray-700"}`}>{label}</span>
      <ArrowUpRight className="w-3.5 h-3.5 text-gray-300 ml-auto opacity-0 group-hover:opacity-100" />
    </Link>
  );
}
