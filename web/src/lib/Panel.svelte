<script lang="ts">
  let { title, subtitle = '', children } = $props()
</script>

<section>
  <h2>
    {title}
    {#if subtitle}<span class="subtitle">{subtitle}</span>{/if}
  </h2>
  {@render children?.()}
</section>

<style>
  section {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 0.75rem;
    padding: 1rem;
  }
  h2 { font-size: 0.95rem; margin: 0 0 0.75rem; display: flex; gap: 0.5rem; align-items: baseline; }
  .subtitle { color: var(--muted); font-weight: 400; font-size: 0.85rem; }
</style>
