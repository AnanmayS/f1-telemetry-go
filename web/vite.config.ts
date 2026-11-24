import { defineConfig } from 'vite';

export default defineConfig({
  server: {
    proxy: { '/api': 'http://127.0.0.1:8090' },
  },
  build: {
    target: 'es2022',
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: { input: { main: 'index.html', predict: 'predict.html' } },
  },
});
