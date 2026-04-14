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

function App() {
  const { user, loading } = useAuth();

  if (loading) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-[var(--color-muted)]">
        <div className="text-[var(--color-muted-foreground)]">Loading...</div>
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
        <Route path="/meters" element={<MetersPage />} />
        <Route path="/plans" element={<PlansPage />} />
        <Route path="/events" element={<EventsPage />} />
        <Route path="/invoices" element={<InvoicesPage />} />
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
