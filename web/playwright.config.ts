import { defineConfig } from '@playwright/test';

// `npm test` builds web/dist first; the Go server then serves the API and the built site.
export default defineConfig({
  testDir: 'tests',
  testMatch: '*.spec.ts',
  timeout: 30_000,
  fullyParallel: true,
  reporter: 'list',
  use: {
    baseURL: 'http://127.0.0.1:8090',
    browserName: 'chromium',
    permissions: ['clipboard-read', 'clipboard-write'],
  },
  webServer: {
    command: './bin/server -addr 127.0.0.1:8090 -data data -static web/dist -rate 0',
    cwd: '..',
    url: 'http://127.0.0.1:8090/api/health',
    reuseExistingServer: true,
    timeout: 30_000,
  },
});
