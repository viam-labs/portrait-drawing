<script lang="ts">
  import type { DrawerStatus } from '../client'
  let { status, onresume }: { status: DrawerStatus | null; onresume: () => void } = $props()

  const alerts: Record<string, { title: string; body: string; action: string }> = {
    excess_force: {
      title: 'Take the cap off the marker',
      body: 'The pen met resistance before it should have reached the paper, so the drawing stopped and the arm moved clear.',
      action: 'Cap is off — resume',
    },
    pressing_too_hard: {
      title: 'The pen pressed too hard',
      body: 'The pen pushed into the paper harder than it should, so the drawing stopped and the arm moved clear. Check the sheet is lying flat and the paper height is right.',
      action: 'Paper checked — resume',
    },
  }
  const alert = $derived(status?.state === 'failed' && status.fault ? alerts[status.fault] : undefined)
  const tripped = $derived(alert !== undefined)
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
      <strong>{alert?.title}</strong>
      <p>{alert?.body}</p>
      {#if status?.last_error}<p class="detail">{status.last_error}</p>{/if}
    </div>
    {#if status?.resumable}
      <button onclick={resume} disabled={resuming}>{resuming ? 'Resuming…' : alert?.action}</button>
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
