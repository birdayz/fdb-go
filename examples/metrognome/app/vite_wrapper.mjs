import { createServer, build } from "vite";

const args = process.argv.slice(2);
if (args[0] === "build") {
  await build();
} else {
  const server = await createServer();
  await server.listen();
  server.printUrls();
}
