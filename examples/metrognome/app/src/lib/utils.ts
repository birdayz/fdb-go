import { type ClassValue, clsx } from "clsx";

export function cn(...inputs: ClassValue[]) {
  return clsx(inputs);
}

export function formatCents(cents: number): string {
  const dollars = cents / 100;
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
  }).format(dollars);
}

export function formatTimestamp(ms: bigint | number): string {
  return new Date(Number(ms)).toLocaleString();
}

export function formatDate(ms: bigint | number): string {
  return new Date(Number(ms)).toLocaleDateString();
}
