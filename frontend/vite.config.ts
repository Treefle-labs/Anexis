import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { resolve } from 'node:path';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    rollupOptions: {
      input: {
        index: resolve(__dirname, 'react-src/index/main.jsx'),
        about: resolve(__dirname, 'react-src/about/main.jsx'),
      },
      output: {
        entryFileNames: 'js/[name].js',
      },
    },
    outDir: 'public',
    emptyOutDir: true,
  },
});
