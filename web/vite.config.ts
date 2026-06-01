import { defineConfig } from "vite";
import solid from "vite-plugin-solid";

// Dev: proxy API + SSE to the Go backend (default :8484).
// Build: emits to ../web/dist, which the Go binary embeds.
const backend = process.env.GK_BACKEND ?? "http://127.0.0.1:8484";

export default defineConfig({
  plugins: [solid()],
  build: { outDir: "dist", emptyOutDir: true },
  server: {
    proxy: {
      "/api": { target: backend, changeOrigin: true },
    },
  },
});
