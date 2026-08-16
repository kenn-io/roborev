<script lang="ts">
  import { onMount, tick } from "svelte";

  import {
    bootstrapSession,
    login,
    sessionLostEvent,
    type SessionCapabilities,
  } from "./lib/api/session";
  import AppShell from "./lib/components/AppShell.svelte";

  type ViewState = "checking" | "login" | "authenticated" | "error";

  let view: ViewState = "checking";
  let token = "";
  let errorMessage = "";
  let connecting = false;
  let retryAfterSeconds = 0;
  let cooldownDeadline = 0;
  let cooldownTimer: ReturnType<typeof setInterval> | undefined;
  let capabilities: SessionCapabilities = {
    cancelAnyJob: false,
    cancelReviewJob: false,
    rerunJob: false,
  };

  onMount(() => {
    document.documentElement.classList.add("dark");
    const handleSessionLost = () => void checkSession();
    globalThis.addEventListener(sessionLostEvent, handleSessionLost);
    void checkSession();
    return () => {
      globalThis.removeEventListener(sessionLostEvent, handleSessionLost);
      stopCooldown();
    };
  });

  function stopCooldown(): void {
    if (cooldownTimer !== undefined) {
      clearInterval(cooldownTimer);
      cooldownTimer = undefined;
    }
  }

  function startCooldown(seconds: number): void {
    stopCooldown();
    cooldownDeadline = Date.now() + Math.max(1, seconds) * 1000;
    updateCooldown();
    cooldownTimer = setInterval(() => {
      updateCooldown();
    }, 1000);
  }

  function updateCooldown(): void {
    retryAfterSeconds = Math.max(
      0,
      Math.ceil((cooldownDeadline - Date.now()) / 1000),
    );
    if (retryAfterSeconds === 0) stopCooldown();
  }

  function clearCooldown(): void {
    stopCooldown();
    cooldownDeadline = 0;
    retryAfterSeconds = 0;
  }

  async function checkSession(): Promise<void> {
    view = "checking";
    const result = await bootstrapSession();
    applySessionResult(result);
  }

  async function connect(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    connecting = true;
    let result: Awaited<ReturnType<typeof login>>;
    try {
      result = await login(token);
    } finally {
      token = "";
      connecting = false;
      await tick();
    }
    applySessionResult(result);
  }

  function applySessionResult(
    result: Awaited<ReturnType<typeof bootstrapSession>>,
  ): void {
    switch (result.state) {
      case "authenticated":
        clearCooldown();
        capabilities = result.capabilities;
        view = "authenticated";
        break;
      case "login-required":
        clearCooldown();
        view = "login";
        break;
      case "rate-limited":
        startCooldown(result.retryAfterSeconds);
        view = "login";
        break;
      case "error":
        clearCooldown();
        errorMessage = result.message;
        view = "error";
        break;
    }
  }
</script>

<main class:application={view === "authenticated"}>
  {#if view === "checking"}
    <section class="card status" aria-live="polite">
      <p class="eyebrow">Roborev</p>
      <p>Checking browser session…</p>
    </section>
  {:else if view === "login"}
    <section class="card login-card">
      <p class="eyebrow">Roborev web</p>
      <h1>Connect to Roborev</h1>
      <p class="muted">
        Enter the browser access token configured for this daemon. It is
        exchanged once and is not retained by the application.
      </p>
      <form onsubmit={connect}>
        <label for="daemon-token">Daemon token</label>
        <input
          id="daemon-token"
          type="password"
          autocomplete="current-password"
          bind:value={token}
          disabled={connecting || retryAfterSeconds > 0}
          required
        />
        {#if retryAfterSeconds > 0}
          <p class="login-warning" role="status">
            Too many attempts. Try again in {retryAfterSeconds}
            {retryAfterSeconds === 1 ? "second" : "seconds"}.
          </p>
        {/if}
        <button type="submit" disabled={connecting || retryAfterSeconds > 0}>
          {connecting
            ? "Connecting…"
            : retryAfterSeconds > 0
              ? `Try again in ${retryAfterSeconds}s`
              : "Connect"}
        </button>
      </form>
    </section>
  {:else if view === "authenticated"}
    <AppShell {capabilities} />
  {:else}
    <section class="card status" role="alert">
      <p class="eyebrow">Connection problem</p>
      <h1>Roborev is unavailable</h1>
      <p>{errorMessage}</p>
      <button type="button" onclick={checkSession}>Retry</button>
    </section>
  {/if}
</main>
