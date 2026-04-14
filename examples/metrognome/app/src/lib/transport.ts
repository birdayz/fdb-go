import { createConnectTransport } from "@connectrpc/connect-web";

export const transport = createConnectTransport({
  baseUrl: "/", // proxied by vite dev server to localhost:8080
});
