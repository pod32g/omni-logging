import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build output is committed and embedded into the Go binary with go:embed,
// so `go build` alone still produces a working server without a Node toolchain.
// Assets are referenced relatively because the UI is served from the binary at
// whatever path the operator mounts it on.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "../dist",
    emptyOutDir: true,
    // Deterministic asset names: the committed dist should only change when the
    // source does, so a stale bundle is visible in review as a real diff.
    rollupOptions: {
      output: {
        entryFileNames: "assets/[name]-[hash].js",
        chunkFileNames: "assets/[name]-[hash].js",
        assetFileNames: "assets/[name]-[hash][extname]",
      },
    },
  },
  server: {
    // `npm run dev` proxies the API to a locally running omnilog.
    proxy: {
      "/api": "http://127.0.0.1:8080",
      "/v1": "http://127.0.0.1:8080",
      "/openapi.json": "http://127.0.0.1:8080",
    },
  },
});
