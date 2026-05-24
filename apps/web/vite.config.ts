import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@kapp/ui": path.resolve(__dirname, "../../packages/ui/src"),
      "@kapp/client": path.resolve(__dirname, "../../packages/client/src"),
    },
  },
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
  build: {
    // Lazy-loaded routes (see App.tsx React.lazy imports) emit one
    // chunk per page automatically — manualChunks below stabilises
    // shared-vendor splitting so the long-tail route chunks don't
    // accidentally re-bundle React / React-Router on every dynamic
    // import.  The shared chunks are named so they're cacheable
    // independently from the route bundles.
    rollupOptions: {
      output: {
        manualChunks: {
          "vendor-react": ["react", "react-dom", "react-router-dom"],
          "vendor-query": ["@tanstack/react-query"],
          "vendor-recharts": ["recharts"],
        },
      },
    },
    // The route-level chunks are small (~5-30KB each); the default
    // 500KB warn threshold is fine for them, but the vendor-react
    // bundle is ~130KB gzipped which is normal — we don't need a
    // bigger limit, just documenting the expected bundle shape.
    chunkSizeWarningLimit: 600,
  },
});
