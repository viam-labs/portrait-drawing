<script lang="ts">
  // The line art is rendered in paper coordinates, which sit 90 degrees from the
  // photo because the arm draws rotated. Showing it upright makes the two panels
  // directly comparable; the toggle still shows what is on the paper.
  let { src, alt }: { src: string; alt: string } = $props()
  let upright = $state(true)
  let naturalWidth = $state(0)
  let naturalHeight = $state(0)

  // Measured rather than assumed: the preview's size follows the paper and its
  // render scale, so a hard-coded aspect ratio would silently misfit if either
  // changed.
  function measure(event: Event & { currentTarget: HTMLImageElement }) {
    naturalWidth = event.currentTarget.naturalWidth
    naturalHeight = event.currentTarget.naturalHeight
  }

  // Rotating swaps the visual axes, so the box takes the inverted ratio.
  const ratio = $derived(
    naturalWidth && naturalHeight
      ? upright
        ? `${naturalHeight} / ${naturalWidth}`
        : `${naturalWidth} / ${naturalHeight}`
      : 'auto',
  )
</script>

<div class="wrap" class:upright style="aspect-ratio: {ratio}">
  <img {src} {alt} onload={measure} />
</div>
<button type="button" onclick={() => (upright = !upright)}>
  {upright ? 'Show as drawn on paper' : 'Match photo orientation'}
</button>

<style>
  .wrap {
    display: grid;
    place-items: center;
    container-type: size;
    background: var(--bg);
    border-radius: 0.5rem;
  }
  img { display: block; max-width: 100%; max-height: 100%; }

  /* A rotated element's visual width comes from its layout height, so the image
     is sized against the container's width — 100cqw, not 100%. Sizing it to the
     container height instead leaves it overflowing and clipped. */
  .upright img {
    height: 100cqw;
    width: auto;
    max-width: none;
    max-height: none;
    /* The pipeline rotates points anti-clockwise to reach paper coordinates
       (rx, ry = y, -x), so undoing it for display is a clockwise turn. */
    transform: rotate(90deg);
  }

  button {
    margin-top: 0.6rem; padding: 0.3rem 0.6rem;
    font: inherit; font-size: 0.8rem;
    color: var(--muted); background: transparent;
    border: 1px solid var(--line); border-radius: 0.4rem; cursor: pointer;
  }
  button:hover { color: var(--text); }
</style>
