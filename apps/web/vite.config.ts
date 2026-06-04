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
        // Content-hashed filenames are the cache-busting contract that
        // the edge/CDN caching layer depends on: bundles emitted under
        // /assets/ are served `Cache-Control: public, max-age=31536000,
        // immutable` (see Caddyfile.prod and
        // internal/platform/cache_control.go), so the [hash] MUST change
        // whenever the bytes change or clients would pin a stale bundle
        // for a year. Vite hashes these by default; the patterns are
        // pinned explicitly so the contract survives future config
        // edits, and everything stays under assets/ to match the
        // /assets/* cache rule (index.html is emitted at the root and
        // intentionally left unhashed — it is served `no-cache`).
        entryFileNames: "assets/[name]-[hash].js",
        chunkFileNames: "assets/[name]-[hash].js",
        assetFileNames: "assets/[name]-[hash][extname]",
      },
    },
    // The route-level chunks are small (~5-30KB each); the default
    // 500KB warn threshold is fine for them, but the vendor-react
    // bundle is ~130KB gzipped which is normal — we don't need a
    // bigger limit, just documenting the expected bundle shape.
    chunkSizeWarningLimit: 600,
  },
});
