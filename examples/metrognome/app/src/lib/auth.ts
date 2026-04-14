import { useState, useEffect } from "react";

export interface User {
  id: string;
  login: string;
  name: string;
  avatar_url: string;
  email: string;
}

export function useAuth() {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    fetch("/auth/me", { credentials: "include" })
      .then((r) => {
        if (r.ok) return r.json();
        throw new Error("not authenticated");
      })
      .then(setUser)
      .catch(() => setUser(null))
      .finally(() => setLoading(false));
  }, []);

  return { user, loading };
}

export function loginWithGitHub() {
  window.location.href = "/auth/login";
}

export async function logout() {
  await fetch("/auth/logout", { method: "POST", credentials: "include" });
  window.location.href = "/";
}
