import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "0.0.0.0", // dentro do contêiner; a porta só é publicada em 127.0.0.1 pelo compose
    port: 5173,
    strictPort: true,
    // O front fala só com /api; o Vite repassa ao BFF. Mesma origem = sem CORS (plano, seção 6.1).
    proxy: {
      "/api": {
        target: process.env.BFF_PROXY_TARGET ?? "http://bff:8088",
        rewrite: (path) => path.replace(/^\/api/, ""),
      },
    },
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
    setupFiles: ["./src/test-setup.ts"],
  },
});
