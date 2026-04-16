import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import "./index.css";
import { useAuth } from "./lib/auth";
import { Layout } from "./components/Layout";
import { LoginPage } from "./pages/Login";
import { DashboardPage } from "./pages/Dashboard";
import { CustomersPage } from "./pages/Customers";
import { MetersPage } from "./pages/Meters";
import { PlansPage } from "./pages/Plans";
import { EventsPage } from "./pages/Events";
import { InvoicesPage } from "./pages/Invoices";
import { CreditsPage } from "./pages/Credits";
import { ProductsPage } from "./pages/Products";
import { RateCardsPage } from "./pages/RateCards";
import { ContractsPage } from "./pages/Contracts";
import { AlertsPage } from "./pages/Alerts";
import { ApiKeysPage } from "./pages/ApiKeys";
import { CustomerDetailPage } from "./pages/CustomerDetail";

function App() {
  const { user, loading } = useAuth();

  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-slate-50">
        <div className="flex flex-col items-center gap-3">
          <div className="w-8 h-8 border-2 border-indigo-600 border-t-transparent rounded-full animate-spin" />
          <span className="text-sm text-gray-400">Loading...</span>
        </div>
      </div>
    );
  }

  if (!user) {
    return <LoginPage />;
  }

  return (
    <Routes>
      <Route element={<Layout user={user} />}>
        <Route path="/" element={<Navigate to="/dashboard" replace />} />
        <Route path="/dashboard" element={<DashboardPage />} />
        <Route path="/customers" element={<CustomersPage />} />
        <Route path="/customers/:id" element={<CustomerDetailPage />} />
        <Route path="/products" element={<ProductsPage />} />
        <Route path="/meters" element={<MetersPage />} />
        <Route path="/rate-cards" element={<RateCardsPage />} />
        <Route path="/plans" element={<PlansPage />} />
        <Route path="/contracts" element={<ContractsPage />} />
        <Route path="/invoices" element={<InvoicesPage />} />
        <Route path="/credits" element={<CreditsPage />} />
        <Route path="/events" element={<EventsPage />} />
        <Route path="/alerts" element={<AlertsPage />} />
        <Route path="/api-keys" element={<ApiKeysPage />} />
      </Route>
    </Routes>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
