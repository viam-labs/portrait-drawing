<script lang="ts">
  import type { DrawerStatus } from '../client'
  let { status, onresume }: { status: DrawerStatus | null; onresume: () => void } = $props()

  const tripped = $derived(status?.state === 'failed' && status.fault === 'excess_force')
  let resuming = $state(false)
  $effect(() => {
    if (!tripped) resuming = false
  })

  function resume() {
    resuming = true
    onresume()
  }
</script>

{#if tripped}
  <section role="alert">
    <div>
      <strong>Take the cap off the marker</strong>
      <p>
        The pen pressed harder than a bare tip should, so the drawing stopped and
        the arm moved clear of the paper.
      </p>
      {#if status?.last_error}<p class="detail">{status.last_error}</p>{/if}
    </div>
    {#if status?.resumable}
      <button onclick={resume} disabled={resuming}>{resuming ? 'Resuming…' : 'Cap is off — resume'}</button>
    {/if}
  </section>
{/if}

<style>
  section {
    display: flex; align-items: center; justify-content: space-between; gap: 1rem; flex-wrap: wrap;
    background: var(--panel); border: 2px solid #dc2626; border-radius: 0.75rem;
    padding: 1rem 1.25rem; margin-bottom: 1rem;
  }
  strong { font-size: 1.15rem; }
  p { margin: 0.25rem 0 0; font-size: 0.9rem; }
  .detail { color: var(--muted); font-size: 0.8rem; }
  button {
    font: inherit; padding: 0.55rem 1rem; border-radius: 0.5rem; cursor: pointer;
    border: 1px solid #dc2626; background: #dc2626; color: #fff;
  }
  button:disabled { opacity: 0.6; cursor: default; }
</style>
