<script lang="ts">
  import { untrack } from "svelte";

  import { KbdBadge, Modal } from "@kenn-io/kit-ui";

  import { pushModalFrame } from "../../keyboard/modal-stack.svelte";

  interface Props {
    open: boolean;
    onclose: () => void;
  }
  let { open, onclose }: Props = $props();

  $effect(() => {
    if (!open) return;
    return untrack(() => pushModalFrame("roborev-shortcut-help", []));
  });
</script>

{#if open}
  <Modal title="Keyboard Shortcuts" {onclose}>
    <div class="shortcuts-body">
      <div class="shortcut-group">
        <h4>Table</h4>
        <dl>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["j"]} /> / <KbdBadge keys={["k"]} /></dt>
            <dd>Move selection down / up</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["Enter"]} /></dt>
            <dd>Open drawer for selected row</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["→"]} /> / <KbdBadge keys={["←"]} /></dt>
            <dd>Expand / collapse review panel</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["x"]} /></dt>
            <dd>Cancel selected job</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["r"]} /></dt>
            <dd>Rerun selected job</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["h"]} /></dt>
            <dd>Toggle hide closed</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["/"]} /></dt>
            <dd>Focus search</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["?"]} /></dt>
            <dd>Toggle this help</dd>
          </div>
        </dl>
      </div>
      <div class="shortcut-group">
        <h4>Drawer</h4>
        <dl>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["Esc"]} /></dt>
            <dd>Close drawer</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["a"]} /></dt>
            <dd>Toggle close / reopen review</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["c"]} /></dt>
            <dd>Focus comment input</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["l"]} /></dt>
            <dd>Switch to Log tab</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["p"]} /></dt>
            <dd>Switch to Prompt tab</dd>
          </div>
          <div class="shortcut-row">
            <dt><KbdBadge keys={["y"]} /></dt>
            <dd>Copy review output</dd>
          </div>
        </dl>
      </div>
    </div>
  </Modal>
{/if}

<style>
  /* kit-ui KbdBadge hides itself on coarse pointers, but this modal only
     opens from a physical keyboard (?), so its badges must stay visible
     on touch-pointer devices that have one attached. */
  .shortcuts-body :global(.kit-kbd-badge) {
    display: inline-flex;
  }

  .shortcuts-body {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 16px;
  }

  .shortcut-group h4 {
    margin: 0 0 8px;
    font-size: var(--font-size-sm);
    font-weight: 600;
    color: var(--text-secondary);
    text-transform: uppercase;
    letter-spacing: 0.5px;
  }

  .shortcut-group dl {
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .shortcut-row {
    display: flex;
    align-items: center;
    gap: 8px;
  }

  .shortcut-row dt {
    flex-shrink: 0;
    min-width: 72px;
    text-align: right;
  }

  .shortcut-row dd {
    margin: 0;
    font-size: var(--font-size-sm);
    color: var(--text-secondary);
  }
</style>
