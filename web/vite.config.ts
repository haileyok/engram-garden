import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The Go server (cmd/engram-web) embeds the build from
// internal/web/dist/app. In development, run engram-web on :8090 with
// ENGRAM_WEB_PUBLIC_URL=http://127.0.0.1:8090 and use `pnpm dev`; API and
// OAuth requests are proxied to it.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/web/dist/app",
    emptyOutDir: true,
  },
  server: {
    host: "127.0.0.1",
    proxy: {
      "/api": "http://127.0.0.1:8090",
      "/oauth": "http://127.0.0.1:8090",
    },
  },
});
