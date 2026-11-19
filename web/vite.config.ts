import { defineConfig } from 'vitest/config';

export default defineConfig({
  server: {
    proxy: {
      '/api': { target: 'http://localhost:8080' },
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
  test: {
    include: ['src/**/*.test.ts'],
  },
});
