<script lang="ts">
  let { state = undefined, drawing = false }: { state?: string; drawing?: boolean } = $props()

  // The wire values are snake_case identifiers; these are for reading.
  const LABELS: Record<string, string> = {
    idle: 'Idle',
    moving_to_capture_pose: 'Moving into position',
    capturing: 'Taking the photo',
    generating_strokes: 'Generating line art',
    drawing: 'Drawing',
    done: 'Finished',
    canceled: 'Canceled',
    failed: 'Failed',
  }
  const label = $derived(state ? (LABELS[state] ?? state) : 'Connecting')
  const tone = $derived(
    state === 'failed' ? 'bad' : state === 'done' ? 'good' : drawing || state === 'capturing' ? 'busy' : 'idle',
  )
</script>

<div class="phase {tone}">
  <span class="dot"></span>
  {label}
</div>

<style>
  .phase {
    display: inline-flex; align-items: center; gap: 0.5rem;
    padding: 0.35rem 0.7rem; border-radius: 999px;
    border: 1px solid var(--line); background: var(--panel);
    font-size: 0.85rem; white-space: nowrap;
  }
  .dot { width: 0.5rem; height: 0.5rem; border-radius: 50%; background: var(--muted); }
  .busy .dot { background: var(--accent); animation: pulse 1.4s ease-in-out infinite; }
  .good .dot { background: #16a34a; }
  .bad .dot { background: #dc2626; }
  @keyframes pulse { 0%, 100% { opacity: 1 } 50% { opacity: 0.25 } }
  @media (prefers-reduced-motion: reduce) { .busy .dot { animation: none } }
</style>
