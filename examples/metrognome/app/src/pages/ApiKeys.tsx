import { useState, useEffect } from "react";
import { createClient } from "@connectrpc/connect";
import { transport } from "@/lib/transport";
import { ApiKeyService } from "@/gen/metrognome/v1/api_key_pb";
import { Key, Plus, Copy, Check, ShieldOff, ShieldCheck, AlertTriangle } from "lucide-react";

const client = createClient(ApiKeyService, transport);

export function ApiKeysPage() {
  const [keys, setKeys] = useState<any[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [name, setName] = useState("");
  const [newKey, setNewKey] = useState<{ id: string; rawKey: string } | null>(null);
  const [copied, setCopied] = useState(false);

  async function load() {
    try {
      const resp = await client.listApiKeys({});
      setKeys(resp.apiKeys);
    } catch (e) { console.error(e); }
    setLoading(false);
  }

  useEffect(() => { load(); }, []);

  async function handleCreate() {
    if (!name.trim()) return;
    try {
      const resp = await client.createApiKey({ name });
      setNewKey({ id: resp.apiKey!.id, rawKey: resp.rawKey });
      setName(""); setShowCreate(false);
      load();
    } catch (e) { console.error(e); }
  }

  async function handleRevoke(id: string) {
    await client.revokeApiKey({ id });
    load();
  }

  function copyKey(key: string) {
    navigator.clipboard.writeText(key);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <div className="p-8 max-w-5xl">
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">API Keys</h1>
          <p className="text-sm text-gray-500 mt-1">Bearer tokens for authenticating API requests. Use these to ingest events programmatically.</p>
        </div>
        <button onClick={() => setShowCreate(true)} className="flex items-center gap-2 px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700 shadow-sm">
          <Plus className="w-4 h-4" /> Create API Key
        </button>
      </div>

      {newKey && (
        <div className="bg-amber-50 border border-amber-200 rounded-xl p-5 mb-6">
          <div className="flex items-start gap-3">
            <AlertTriangle className="w-5 h-5 text-amber-600 mt-0.5 shrink-0" />
            <div className="flex-1">
              <h3 className="font-semibold text-amber-900 mb-1">Save your API key</h3>
              <p className="text-sm text-amber-700 mb-3">This key will only be shown once. Copy it now and store it securely.</p>
              <div className="flex items-center gap-2">
                <code className="flex-1 px-3 py-2 bg-white border border-amber-300 rounded-lg text-sm font-mono text-gray-900 select-all">{newKey.rawKey}</code>
                <button onClick={() => copyKey(newKey.rawKey)}
                  className="flex items-center gap-1 px-3 py-2 bg-amber-600 text-white rounded-lg text-sm font-medium hover:bg-amber-700">
                  {copied ? <Check className="w-4 h-4" /> : <Copy className="w-4 h-4" />}
                  {copied ? "Copied" : "Copy"}
                </button>
              </div>
              <div className="mt-3 text-xs text-amber-600">
                Usage: <code className="bg-white px-1.5 py-0.5 rounded border border-amber-200">curl -H "Authorization: Bearer {newKey.rawKey.slice(0, 12)}..." ...</code>
              </div>
            </div>
          </div>
          <button onClick={() => setNewKey(null)} className="mt-3 text-xs text-amber-600 hover:text-amber-800 font-medium">Dismiss</button>
        </div>
      )}

      {showCreate && (
        <div className="bg-white rounded-xl border border-gray-200 p-6 mb-6 shadow-sm">
          <h3 className="font-semibold text-gray-900 mb-4">New API Key</h3>
          <div>
            <label className="block text-sm font-medium text-gray-700 mb-1">Name <span className="text-red-400">*</span></label>
            <input value={name} onChange={e => setName(e.target.value)} placeholder="e.g. Production Ingest, CI Pipeline"
              className="w-full max-w-md px-3 py-2 border border-gray-300 rounded-lg text-sm focus:ring-2 focus:ring-indigo-500 focus:border-transparent outline-none" />
          </div>
          <div className="flex gap-2 mt-4">
            <button onClick={handleCreate} className="px-4 py-2 bg-indigo-600 text-white rounded-lg text-sm font-medium hover:bg-indigo-700">Create</button>
            <button onClick={() => setShowCreate(false)} className="px-4 py-2 text-gray-600 rounded-lg text-sm hover:bg-gray-100">Cancel</button>
          </div>
        </div>
      )}

      {loading ? (
        <div className="text-center py-12 text-gray-400">Loading...</div>
      ) : keys.length === 0 ? (
        <div className="text-center py-16 bg-white rounded-xl border border-gray-200">
          <Key className="w-12 h-12 text-gray-300 mx-auto mb-3" />
          <p className="text-gray-500 font-medium">No API keys yet</p>
          <p className="text-sm text-gray-400 mt-1">Create an API key to start ingesting events programmatically.</p>
        </div>
      ) : (
        <div className="space-y-3">
          {keys.map((k: any) => (
            <div key={k.id} className="bg-white rounded-xl border border-gray-200 p-5 shadow-sm">
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-3">
                  <div className={`w-10 h-10 rounded-lg flex items-center justify-center ${k.revoked ? "bg-red-50" : "bg-emerald-50"}`}>
                    {k.revoked ? <ShieldOff className="w-5 h-5 text-red-500" /> : <ShieldCheck className="w-5 h-5 text-emerald-600" />}
                  </div>
                  <div>
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-semibold text-gray-900">{k.name}</span>
                      {k.revoked && (
                        <span className="px-2 py-0.5 rounded-full text-xs font-semibold bg-red-100 text-red-700 border border-red-200">Revoked</span>
                      )}
                    </div>
                    <code className="text-xs text-gray-400 font-mono">{k.keyPrefix}</code>
                  </div>
                </div>
                <div className="flex items-center gap-3">
                  {!k.revoked && (
                    <button onClick={() => handleRevoke(k.id)}
                      className="px-3 py-1.5 text-xs font-medium text-red-600 hover:bg-red-50 rounded-lg border border-red-200">
                      Revoke
                    </button>
                  )}
                </div>
              </div>
              <div className="flex gap-4 mt-3 text-xs text-gray-500">
                <span>ID: <strong className="font-mono text-gray-600">{k.id}</strong></span>
                {Number(k.lastUsedAt) > 0 && (
                  <span>Last used: <strong>{new Date(Number(k.lastUsedAt)).toLocaleString()}</strong></span>
                )}
                <span className="ml-auto">Created {new Date(Number(k.createdAt)).toLocaleDateString()}</span>
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
