import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { AlertService } from "@/gen/metrognome/v1/alert_pb";
import { CustomerService } from "@/gen/metrognome/v1/customer_pb";
import { MeterService } from "@/gen/metrognome/v1/meter_pb";
import { Bell, Plus, Search, AlertTriangle, CheckCircle2, Activity, Globe } from "lucide-react";

const alertClient = createClient(AlertService, transport);
const customerClient = createClient(CustomerService, transport);
const meterClient = createClient(MeterService, transport);

const alertTypeLabels: Record<number, { label: string; color: string }> = {
  1: { label: "Usage", color: "bg-blue-100 text-blue-700" },
  2: { label: "Spend", color: "bg-amber-100 text-amber-700" },
};

export function AlertsPage() {
  const [customers, setCustomers] = useState<any[]>([]);
  const [meters, setMeters] = useState<any[]>([]);
  const [customerId, setCustomerId] = useState("");
  const [alerts, setAlerts] = useState<any[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [showCreate, setShowCreate] = useState(false);

  // Create form
  const [createCustomerId, setCreateCustomerId] = useState("");
  const [createMeterSlug, setCreateMeterSlug] = useState("");
  const [createThreshold, setCreateThreshold] = useState("");
  const [createAlertType, setCreateAlertType] = useState(1);
  const [createWebhookUrl, setCreateWebhookUrl] = useState("");

  useEffect(() => {
    Promise.all([
      customerClient.listCustomers({}).then(r => setCustomers(r.customers)),
      meterClient.listMeters({}).then(r => setMeters(r.meters)),
    ]).catch(() => {});
  }, []);

  async function search() {
    if (!customerId) return;
    try {
      const resp = await alertClient.listAlerts({ customerId });
      setAlerts(resp.alerts);
    } catch (e) { setAlerts([]); }
    setLoaded(true);
  }

  async function handleCreate() {
    if (!createCustomerId || !createMeterSlug || !createThreshold) return;
    await alertClient.createAlert({
      customerId: createCustomerId,
      meterSlug: createMeterSlug,
      threshold: BigInt(parseInt(createThreshold)),
      alertType: createAlertType,
      webhookUrl: createWebhookUrl || "",
    });
    setShowCreate(false);
    setCreateCustomerId(""); setCreateMeterSlug(""); setCreateThreshold(""); setCreateWebhookUrl("");
    if (createCustomerId === customerId) search();
  }

  return (
    <div className="p-8 max-w-5xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Alerts</h1>
          <p className="text-sm text-gray-500 mt-1">Threshold notifications on usage and spend. Optionally deliver via webhook.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Create Alert
        </button>
      </div>

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New Alert</h3>
          <div className="grid grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Customer <span className="text-red-400">*</span></label>
              <select value={createCustomerId} onChange={e => setCreateCustomerId(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select customer...</option>
                {customers.map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Meter <span className="text-red-400">*</span></label>
              <select value={createMeterSlug} onChange={e => setCreateMeterSlug(e.target.value)}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value="">Select meter...</option>
                {meters.map(m => <option key={m.slug} value={m.slug}>{m.name} ({m.slug})</option>)}
              </select>
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Threshold <span className="text-red-400">*</span></label>
              <input type="number" value={createThreshold} onChange={e => setCreateThreshold(e.target.value)} placeholder="e.g. 10000"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
            <div>
              <label className="block text-sm font-medium text-gray-700 mb-1">Alert Type</label>
              <select value={createAlertType} onChange={e => setCreateAlertType(Number(e.target.value))}
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none">
                <option value={1}>Usage Threshold</option>
                <option value={2}>Spend Threshold</option>
              </select>
            </div>
            <div className="col-span-2">
              <label className="block text-sm font-medium text-gray-700 mb-1">Webhook URL (optional)</label>
              <input value={createWebhookUrl} onChange={e => setCreateWebhookUrl(e.target.value)} placeholder="https://example.com/webhook"
                className="w-full px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
            </div>
          </div>
          <div className="flex gap-2 mt-4">
            <button onClick={handleCreate} className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700">Create</button>
            <button onClick={() => setShowCreate(false)} className="px-4 py-2 text-gray-600 rounded-lg text-sm hover:bg-gray-100">Cancel</button>
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
            <Search className="w-4 h-4" /> View Alerts
          </button>
        </div>
      </div>

      {!loaded ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Bell className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">Select a customer to view their alerts</p>
        </div>
      ) : alerts.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <AlertTriangle className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No alerts found</p>
          <p className="text-sm text-gray-400 mt-1">Create an alert to monitor this customer's usage.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {alerts.map((al: any) => {
            const at = alertTypeLabels[al.alertType] || { label: "Unknown", color: "bg-gray-100 text-gray-600" };
            return (
              <div key={al.id} className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
                <div className="flex items-start justify-between">
                  <div className="flex items-center gap-3">
                    <div className={`w-10 h-10 rounded-lg flex items-center justify-center ${al.triggered ? "bg-red-50" : "bg-emerald-50"}`}>
                      {al.triggered ? <AlertTriangle className="w-5 h-5 text-red-600" /> : <Bell className="w-5 h-5 text-emerald-600" />}
                    </div>
                    <div>
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-semibold text-gray-900">{al.meterSlug}</span>
                        <span className={`px-2 py-0.5 rounded-full text-xs font-semibold ${at.color}`}>{at.label}</span>
                      </div>
                      <div className="text-xs text-gray-400 font-mono mt-0.5">{al.id}</div>
                    </div>
                  </div>
                  <div className="text-right">
                    {al.triggered ? (
                      <span className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-semibold bg-red-100 text-red-800 border border-red-200">
                        <AlertTriangle className="w-3 h-3" /> Triggered
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-1 px-2.5 py-1 rounded-full text-xs font-semibold bg-emerald-100 text-emerald-800 border border-emerald-200">
                        <CheckCircle2 className="w-3 h-3" /> Active
                      </span>
                    )}
                  </div>
                </div>

                <div className="flex gap-6 mt-3 text-xs text-gray-500">
                  <span className="flex items-center gap-1">
                    <Activity className="w-3 h-3" />
                    Threshold: <strong className="text-gray-700">{Number(al.threshold).toLocaleString()}</strong>
                  </span>
                  {al.webhookUrl && (
                    <span className="flex items-center gap-1">
                      <Globe className="w-3 h-3" />
                      <span className="text-gray-700 truncate max-w-[200px]">{al.webhookUrl}</span>
                    </span>
                  )}
                  {al.triggered && Number(al.triggeredAt) > 0 && (
                    <span>Triggered: <strong>{new Date(Number(al.triggeredAt)).toLocaleString()}</strong></span>
                  )}
                  <span className="ml-auto">Created {new Date(Number(al.createdAt)).toLocaleDateString()}</span>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
