import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

export default defineConfig({
  plugins: [svelte()],
  // Viam serves an application's bundle from a sub-path, so asset URLs have to
  // be relative. With the default absolute base nothing loads once deployed,
  // while everything still works locally — an easy way to ship a broken app.
  base: './',
  build: { outDir: './dist', emptyOutDir: true },
})
