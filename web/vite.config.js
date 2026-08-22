import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', sourcemap: false },
  server: {
    port: 5173,
    // Local development talks to a port-forwarded backend:
    //   kubectl -n k8s-lab port-forward svc/app-k8s-lab-app 18080:80
    proxy: { '/api': { target: 'http://127.0.0.1:18080', rewrite: (p) => p.replace(/^\/api/, '') } },
  },
});
