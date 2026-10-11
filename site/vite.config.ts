import { defineConfig } from 'vite';
import { resolve } from 'node:path';

export default defineConfig({
  base: '/conductor/',
  build: {
    rollupOptions: {
      input: {
        main: resolve(__dirname, 'index.html'),
        docs: resolve(__dirname, 'docs/index.html'),
        'docs-quickstart': resolve(__dirname, 'docs/quickstart.html'),
        'docs-daily-use': resolve(__dirname, 'docs/daily-use.html'),
        'docs-concepts': resolve(__dirname, 'docs/concepts.html'),
        'docs-configuration': resolve(__dirname, 'docs/configuration.html'),
        'docs-meshing': resolve(__dirname, 'docs/meshing.html'),
        'docs-access': resolve(__dirname, 'docs/access.html'),
      },
    },
  },
});
