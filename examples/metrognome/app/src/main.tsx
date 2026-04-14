import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Routes, Route, Navigate } from "react-router-dom";
import "./index.css";
import { Layout } from "./components/Layout";
import { DashboardPage } from "./pages/Dashboard";
import { CustomersPage } from "./pages/Customers";
import { MetersPage } from "./pages/Meters";
import { PlansPage } from "./pages/Plans";
import { EventsPage } from "./pages/Events";
import { InvoicesPage } from "./pages/Invoices";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route path="/" element={<Navigate to="/dashboard" replace />} />
          <Route path="/dashboard" element={<DashboardPage />} />
          <Route path="/customers" element={<CustomersPage />} />
          <Route path="/meters" element={<MetersPage />} />
          <Route path="/plans" element={<PlansPage />} />
          <Route path="/events" element={<EventsPage />} />
          <Route path="/invoices" element={<InvoicesPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  </StrictMode>,
);
