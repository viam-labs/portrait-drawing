<script lang="ts">
  import { machineTarget } from './machine'

  const target = machineTarget()
</script>

<main>
  <header>
    <h1>Portrait Drawing</h1>
    <p class="muted">Live view of the arm's capture and drawing</p>
  </header>

  {#if target}
    <section class="panel">
      <h2>Connected</h2>
      <dl>
        <dt>machine</dt><dd>{target.host}</dd>
      </dl>
      <p class="muted">
        Capture feedback, the photo, the generated line art and drawing progress
        land here next.
      </p>
    </section>
  {:else}
    <section class="panel">
      <h2>No machine</h2>
      <p class="muted">
        This page expects to be opened from its Viam application, which supplies
        the machine and its credentials. Opened directly, it has nothing to
        connect to.
      </p>
    </section>
  {/if}
</main>

<style>
  main { max-width: 60rem; margin: 0 auto; padding: 2rem 1.5rem; }
  header { margin-bottom: 1.5rem; }
  h1 { font-size: 1.5rem; margin: 0 0 0.25rem; }
  h2 { font-size: 1rem; margin: 0 0 0.75rem; }
  .muted { color: var(--muted); margin: 0.25rem 0 0; font-size: 0.9rem; }
  .panel {
    background: var(--panel);
    border: 1px solid var(--line);
    border-radius: 0.75rem;
    padding: 1.25rem;
  }
  dl { display: grid; grid-template-columns: auto 1fr; gap: 0.25rem 1rem; margin: 0; font-size: 0.9rem; }
  dt { color: var(--muted); }
  dd { margin: 0; font-family: ui-monospace, SFMono-Regular, monospace; }
</style>
