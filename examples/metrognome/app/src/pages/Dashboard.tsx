import { useEffect, useState } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import { PlanService } from "@/gen/metrognome/v1/plan_pb";

export function DashboardPage() {
  const [stats, setStats] = useState({ customers: 0, meters: 0, plans: 0 });

  useEffect(() => {
    async function load() {
      const customerClient = createClient(CustomerService, transport);
      const meterClient = createClient(MeterService, transport);
      const planClient = createClient(PlanService, transport);

      const [customers, meters, plans] = await Promise.all([
        customerClient.listCustomers({}),
        meterClient.listMeters({}),
        planClient.listPlans({}),
      ]);

      setStats({
        customers: customers.customers.length,
        meters: meters.meters.length,
        plans: plans.plans.length,
      });
    }
    load();
  }, []);

  return (
    <div>
      <h2 className="text-2xl font-bold mb-6">Dashboard</h2>
      <div className="grid grid-cols-3 gap-6">
        <StatCard label="Customers" value={stats.customers} />
        <StatCard label="Meters" value={stats.meters} />
        <StatCard label="Plans" value={stats.plans} />
      </div>
    </div>
  );
}

function StatCard({ label, value }: { label: string; value: number }) {
  return (
    <div className="bg-white rounded-xl border border-[var(--color-border)] p-6">
      <p className="text-sm text-[var(--color-muted-foreground)]">{label}</p>
      <p className="text-3xl font-bold mt-2">{value}</p>
    </div>
  );
}
