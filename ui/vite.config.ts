import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// During development run the Go host on :8080 and `npm run dev` here;
// API calls are proxied to the host.
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true },
  server: {
    proxy: { "/api": "http://127.0.0.1:8080" },
  },
});
