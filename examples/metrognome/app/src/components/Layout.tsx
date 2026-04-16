import { useState } from "react";
import { Outlet, NavLink, useLocation } from "react-router-dom";
import {
  LayoutDashboard,
  Users,
  Gauge,
  CreditCard,
  Zap,
  FileText,
  LogOut,
  ChevronDown,
  Package,
  Tag,
  Layers,
  DollarSign,
  Key,
  Bell,
  Settings,
  Activity,
  BarChart3,
  ScrollText,
} from "lucide-react";
import type { User } from "@/lib/auth";
import { logout } from "@/lib/auth";
import { logoUrl } from "@/lib/logo";

interface NavSection {
  label: string;
  items: NavItem[];
  defaultOpen?: boolean;
}

interface NavItem {
  to: string;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
}

const navSections: NavSection[] = [
  {
    label: "",
    defaultOpen: true,
    items: [
      { to: "/dashboard", label: "Dashboard", icon: LayoutDashboard },
      { to: "/customers", label: "Customers", icon: Users },
    ],
  },
  {
    label: "Offering",
    defaultOpen: true,
    items: [
      { to: "/products", label: "Products", icon: Package },
      { to: "/meters", label: "Billable Metrics", icon: Gauge },
      { to: "/rate-cards", label: "Rate Cards", icon: DollarSign },
      { to: "/plans", label: "Plans", icon: Layers },
    ],
  },
  {
    label: "Billing",
    defaultOpen: true,
    items: [
      { to: "/contracts", label: "Contracts", icon: ScrollText },
      { to: "/invoices", label: "Invoices", icon: FileText },
      { to: "/credits", label: "Credits", icon: CreditCard },
    ],
  },
  {
    label: "Connections",
    defaultOpen: false,
    items: [
      { to: "/events", label: "Events", icon: Zap },
      { to: "/alerts", label: "Alerts", icon: Bell },
      { to: "/api-keys", label: "API Keys", icon: Key },
    ],
  },
];

function SidebarSection({ section }: { section: NavSection }) {
  const location = useLocation();
  const isAnyActive = section.items.some((item) =>
    location.pathname.startsWith(item.to)
  );
  const [open, setOpen] = useState(section.defaultOpen || isAnyActive);

  if (!section.label) {
    return (
      <div className="space-y-0.5">
        {section.items.map((item) => (
          <SidebarLink key={item.to} item={item} />
        ))}
      </div>
    );
  }

  return (
    <div>
      <button
        onClick={() => setOpen(!open)}
        className="flex items-center justify-between w-full px-3 py-1.5 text-[11px] font-semibold uppercase tracking-wider text-indigo-300/60 hover:text-indigo-200/80"
      >
        {section.label}
        <ChevronDown
          className={`w-3 h-3 transition-transform ${open ? "" : "-rotate-90"}`}
        />
      </button>
      {open && (
        <div className="space-y-0.5 mt-0.5">
          {section.items.map((item) => (
            <SidebarLink key={item.to} item={item} />
          ))}
        </div>
      )}
    </div>
  );
}

function SidebarLink({ item }: { item: NavItem }) {
  return (
    <NavLink
      to={item.to}
      className={({ isActive }) =>
        `flex items-center gap-2.5 px-3 py-1.5 rounded-md text-[13px] font-medium transition-all ${
          isActive
            ? "bg-indigo-600 text-white shadow-sm shadow-indigo-900/30"
            : "text-indigo-200/70 hover:bg-indigo-800/30 hover:text-indigo-100"
        }`
      }
    >
      <item.icon className="w-4 h-4 shrink-0" />
      {item.label}
    </NavLink>
  );
}

export function Layout({ user }: { user: User }) {
  return (
    <div className="flex h-screen">
      {/* Sidebar */}
      <nav className="w-56 bg-[#1e1b4b] flex flex-col shrink-0">
        {/* Logo */}
        <div className="px-4 py-5 flex items-center gap-2.5">
          <img src={logoUrl} alt="Metrognome" className="w-8 h-8 rounded-lg" />
          <div>
            <h1 className="text-[15px] font-bold text-white leading-none">
              Metrognome
            </h1>
            <p className="text-[10px] text-indigo-300/50 mt-0.5">
              Usage-Based Billing
            </p>
          </div>
        </div>

        {/* Navigation */}
        <div className="flex-1 px-2 py-2 space-y-3 overflow-y-auto">
          {navSections.map((section, i) => (
            <SidebarSection key={i} section={section} />
          ))}
        </div>

        {/* User */}
        <div className="px-3 py-3 border-t border-indigo-800/50">
          <div className="flex items-center gap-2.5">
            <img
              src={user.avatar_url}
              alt={user.login}
              className="w-7 h-7 rounded-full ring-2 ring-indigo-700/50"
            />
            <div className="flex-1 min-w-0">
              <p className="text-[13px] font-medium text-indigo-100 truncate">
                {user.name || user.login}
              </p>
              <p className="text-[11px] text-indigo-300/50 truncate">
                {user.login}
              </p>
            </div>
            <button
              onClick={logout}
              className="p-1 text-indigo-400/50 hover:text-red-400 rounded transition-colors"
              title="Sign out"
            >
              <LogOut className="w-3.5 h-3.5" />
            </button>
          </div>
        </div>
      </nav>

      {/* Main content */}
      <main className="flex-1 overflow-auto bg-[#f8fafc]">
        <Outlet />
      </main>
    </div>
  );
}
