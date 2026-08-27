<script lang="ts">
  // The line art is rendered in paper coordinates, which sit 90 degrees from the
  // photo because the arm draws rotated. Showing it upright makes the two
  // panels directly comparable; the toggle still shows what is on the paper.
  let { src, alt }: { src: string; alt: string } = $props()
  let upright = $state(true)
</script>

<div class="wrap" class:upright>
  <img {src} {alt} />
</div>
<button type="button" onclick={() => (upright = !upright)}>
  {upright ? 'Show as drawn on paper' : 'Match photo orientation'}
</button>

<style>
  .wrap { display: grid; place-items: center; overflow: hidden; }
  img { width: 100%; display: block; border-radius: 0.5rem; background: var(--bg); }

  /* Rotating swaps the box's axes, so the wrapper takes the inverted aspect and
     the image is sized against it — otherwise the rotated image overflows. */
  .upright { aspect-ratio: 279.4 / 215.9; }
  .upright img {
    width: auto;
    height: 100%;
    max-width: none;
    transform: rotate(-90deg);
    transform-origin: center;
  }

  button {
    margin-top: 0.6rem; padding: 0.3rem 0.6rem;
    font: inherit; font-size: 0.8rem;
    color: var(--muted); background: transparent;
    border: 1px solid var(--line); border-radius: 0.4rem; cursor: pointer;
  }
  button:hover { color: var(--text); }
</style>
