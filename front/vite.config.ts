import { fileURLToPath, URL } from "node:url";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
	plugins: [react()],
	resolve: {
		alias: {
			"@": fileURLToPath(new URL("./src", import.meta.url)),
		},
	},
	server: {
		port: 5173,
		proxy: {
			// Dev talks to the SAME edge as prod: Caddy on :80. Hitting ucapi
			// directly is wrong (it publishes no host port), and a different
			// origin would drop the HttpOnly same-site session cookie.
			"/v1": { target: "http://localhost", changeOrigin: true },
			"/public": { target: "http://localhost", changeOrigin: true },
			// Exact match on purpose: a bare "/i" prefix would swallow the app's
			// own /incidents route.
			"^/i$": { target: "http://localhost", changeOrigin: true },
			"/hooks": { target: "http://localhost", changeOrigin: true },
			"/health": { target: "http://localhost", changeOrigin: true },
		},
	},
});
