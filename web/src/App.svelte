<script lang="ts">
  import { onDestroy, onMount } from 'svelte'
  import type { RobotClient } from '@viamrobotics/sdk'
  import { lookedFor, machineTarget } from './machine'
  import { connect, drawerStatus, frameURL, type DrawerStatus } from './client'
  import Panel from './lib/Panel.svelte'
  import Progress from './lib/Progress.svelte'
  import Phase from './lib/Phase.svelte'
  import Rotatable from './lib/Rotatable.svelte'

  const POLL_MS = 1000

  const target = machineTarget()
  let client: RobotClient | null = $state(null)
  let error: string | null = $state(null)
  let statusError: string | null = $state(null)
  let status: DrawerStatus | null = $state(null)
  let photo: string | null = $state(null)
  let lineArt: string | null = $state(null)
  let timer: ReturnType<typeof setInterval> | undefined

  // Object URLs are revoked as they are replaced; letting them accumulate over a
  // dashboard left open for hours is a slow leak.
  function swap(previous: string | null, next: string | null): string | null {
    if (previous && previous !== next) URL.revokeObjectURL(previous)
    return next
  }

  async function refresh(c: RobotClient) {
    // Each source is independent. Losing status should not blank the photo and
    // line art, which are the point of the dashboard — a drawer too old to know
    // the status verb still draws, and its cameras still hold frames.
    try {
      status = await drawerStatus(c, 'drawer')
      statusError = null
    } catch (e) {
      statusError = e instanceof Error ? e.message : String(e)
      status = null
    }
    // A camera holding nothing is normal, not a failure: before the first
    // capture, and after any module restart, since the buffer is in memory.
    photo = swap(photo, await frameURL(c, 'photo').catch(() => null))
    lineArt = swap(lineArt, await frameURL(c, 'line-preview').catch(() => null))
  }

  onMount(async () => {
    if (!target) return
    try {
      client = await connect(target)
    } catch (e) {
      error = e instanceof Error ? e.message : String(e)
      return
    }
    await refresh(client)
    timer = setInterval(() => client && refresh(client), POLL_MS)
  })

  onDestroy(() => {
    clearInterval(timer)
    swap(photo, null)
    swap(lineArt, null)
  })
</script>

<main>
  <header>
    <div>
      <h1>Portrait Drawing</h1>
      <p class="muted">
        {#if target}{target.host}{:else}not connected{/if}
        {#if target && target.source !== 'viam'}<span class="tag">{target.source}</span>{/if}
      </p>
    </div>
    <Phase state={status?.state} drawing={status?.drawing} />
  </header>

  {#if !target}
    <Panel title="No machine">
      <p class="muted">
        This page gets its machine and credentials from Viam when opened through
        the application. It looked for {lookedFor()} and found nothing.
      </p>
      <p class="muted">
        For local development, set <code>web/.env.local</code>.
      </p>
    </Panel>
  {:else if error}
    <Panel title="Cannot reach the machine">
      <p class="muted">{error}</p>
    </Panel>
  {:else}
    {#if statusError}
      <Panel title="No progress available">
        <p class="muted">{statusError}</p>
      </Panel>
    {/if}
    <Progress {status} />
    <div class="grid">
      <Panel title="Photo" subtitle="what the camera captured">
        {#if photo}
          <img src={photo} alt="the captured photo" />
        {:else}
          <p class="muted">Nothing captured yet.</p>
        {/if}
      </Panel>
      <Panel title="Line art" subtitle="what the arm is drawing">
        {#if lineArt}
          <Rotatable src={lineArt} alt="the generated line drawing" />
        {:else}
          <p class="muted">Nothing generated yet.</p>
        {/if}
      </Panel>
    </div>
  {/if}
</main>

<style>
  main { max-width: 68rem; margin: 0 auto; padding: 2rem 1.5rem; }
  header {
    display: flex; align-items: flex-start; justify-content: space-between;
    gap: 1rem; margin-bottom: 1.5rem; flex-wrap: wrap;
  }
  h1 { font-size: 1.5rem; margin: 0 0 0.25rem; }
  .muted { color: var(--muted); margin: 0; font-size: 0.9rem; }
  .tag {
    display: inline-block; margin-left: 0.5rem; padding: 0.1rem 0.45rem;
    border: 1px solid var(--line); border-radius: 0.35rem; font-size: 0.75rem;
  }
  code { font-family: ui-monospace, SFMono-Regular, monospace; font-size: 0.85em; }
  .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(20rem, 1fr)); gap: 1rem; margin-top: 1rem; }
  img { width: 100%; border-radius: 0.5rem; display: block; background: var(--bg); }
</style>
