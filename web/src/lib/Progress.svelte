<script lang="ts">
  import type { DrawerStatus } from '../client'
  let { status }: { status: DrawerStatus | null } = $props()

  const total = $derived(status?.polylines_total ?? 0)
  const done = $derived(status?.polylines_done ?? 0)
  const percent = $derived(status?.percent ?? 0)

  function clock(seconds?: number): string {
    if (seconds === undefined) return '—'
    const s = Math.round(seconds)
    return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`
  }

  // Remaining is extrapolated from strokes finished so far, so it is meaningless
  // until a few are done and is not shown before then.
  const remaining = $derived(
    status?.drawing && done > 2 && status.elapsed_sec
      ? clock((status.elapsed_sec / done) * (total - done))
      : null,
  )
</script>

{#if total > 0}
  <section>
    <div class="row">
      <strong>{done} / {total} strokes</strong>
      <span class="muted">
        {clock(status?.elapsed_sec)} elapsed{#if remaining} · about {remaining} left{/if}
      </span>
    </div>
    <div class="track" role="progressbar" aria-valuenow={percent} aria-valuemin="0" aria-valuemax="100">
      <div class="fill" style="width: {percent}%"></div>
    </div>
  </section>
{/if}

<style>
  section {
    background: var(--panel); border: 1px solid var(--line);
    border-radius: 0.75rem; padding: 1rem;
  }
  .row { display: flex; justify-content: space-between; gap: 1rem; flex-wrap: wrap; margin-bottom: 0.6rem; font-size: 0.9rem; }
  .muted { color: var(--muted); }
  .track { height: 0.5rem; border-radius: 999px; background: var(--line); overflow: hidden; }
  .fill { height: 100%; background: var(--accent); transition: width 0.4s ease; }
</style>
