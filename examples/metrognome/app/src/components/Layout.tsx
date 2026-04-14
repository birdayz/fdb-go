import { Outlet, NavLink } from "react-router-dom";
import {
  LayoutDashboard,
  Users,
  Gauge,
  CreditCard,
  Zap,
  FileText,
} from "lucide-react";

const navItems = [
  { to: "/dashboard", label: "Dashboard", icon: LayoutDashboard },
  { to: "/customers", label: "Customers", icon: Users },
  { to: "/meters", label: "Meters", icon: Gauge },
  { to: "/plans", label: "Plans", icon: CreditCard },
  { to: "/events", label: "Events", icon: Zap },
  { to: "/invoices", label: "Invoices", icon: FileText },
];

export function Layout() {
  return (
    <div className="flex h-screen">
      {/* Sidebar */}
      <nav className="w-64 bg-white border-r border-[var(--color-border)] flex flex-col">
        <div className="p-6 border-b border-[var(--color-border)]">
          <h1 className="text-xl font-bold text-[var(--color-primary)]">
            Metrognome
          </h1>
          <p className="text-xs text-[var(--color-muted-foreground)] mt-1">
            Usage-Based Billing
          </p>
        </div>
        <div className="flex-1 p-4 space-y-1">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              className={({ isActive }) =>
                `flex items-center gap-3 px-3 py-2 rounded-lg text-sm transition-colors ${
                  isActive
                    ? "bg-[var(--color-primary)] text-white"
                    : "text-[var(--color-muted-foreground)] hover:bg-[var(--color-muted)]"
                }`
              }
            >
              <item.icon className="w-4 h-4" />
              {item.label}
            </NavLink>
          ))}
        </div>
      </nav>

      {/* Main content */}
      <main className="flex-1 overflow-auto p-8">
        <Outlet />
      </main>
    </div>
  );
}
