/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In dev, /api and /auth go to a locally running portal-api (make run, port 8080).
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", assetsInlineLimit: 0 }, // no inline assets: the CSP forbids data: URLs
  server: { proxy: { "/api": "http://localhost:8080", "/auth": "http://localhost:8080" } },
  test: { environment: "jsdom", globals: true, setupFiles: ["src/setupTests.ts"] },
});
