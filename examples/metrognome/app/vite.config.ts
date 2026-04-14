import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";
import path from "node:path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "src"),
    },
  },
  server: {
    port: 3000,
    proxy: {
      // Proxy ConnectRPC requests to the Go backend
      "/metrognome.v1": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
      // Proxy auth routes to the Go backend
      "/auth": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
});
