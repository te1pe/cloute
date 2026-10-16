import { defineConfig } from 'vitest/config';

export default defineConfig({
  server: {
    // During development the Go server runs separately on :8080.
    proxy: {
      '/api': { target: 'http://localhost:8080' },
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
  test: {
    include: ['src/**/*.test.ts'],
  },
});
