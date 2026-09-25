import "@testing-library/jest-dom/vitest";

import { cleanup } from "@testing-library/svelte";
import { afterEach } from "vitest";

// CI's runner Node predates Promise.withResolvers (Node 22), which tests use.
if (!("withResolvers" in Promise)) {
  Object.defineProperty(Promise, "withResolvers", {
    configurable: true,
    writable: true,
    value: <T>(): PromiseWithResolvers<T> => {
      let resolve!: (value: T | PromiseLike<T>) => void;
      let reject!: (reason?: unknown) => void;
      const promise = new Promise<T>((res, rej) => {
        resolve = res;
        reject = rej;
      });
      return { promise, resolve, reject };
    },
  });
}

// Bun exposes an optional process-global localStorage whose undefined value can
// shadow jsdom's origin-scoped implementation. Supply a standards-shaped test
// store when that happens.
if (typeof window !== "undefined") {
  const localStorage = window.localStorage ?? createMemoryStorage();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: localStorage,
  });
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    value: localStorage,
  });
  Object.defineProperty(globalThis, "sessionStorage", {
    configurable: true,
    value: window.sessionStorage,
  });
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: (query: string): MediaQueryList => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener() {},
      removeEventListener() {},
      addListener() {},
      removeListener() {},
      dispatchEvent: () => false,
    }),
  });
}

if (typeof globalThis.crypto !== "undefined") {
  let nonce = 0;
  const testCrypto = Object.create(globalThis.crypto) as Crypto;
  Object.defineProperties(testCrypto, {
    randomUUID: {
      configurable: true,
      value: () =>
        `00000000-0000-4000-8000-${String(++nonce).padStart(12, "0")}`,
    },
    getRandomValues: {
      configurable: true,
      value: <T extends ArrayBufferView | null>(array: T): T => {
        if (array === null) return array;
        const bytes = new Uint8Array(
          array.buffer,
          array.byteOffset,
          array.byteLength,
        );
        bytes.fill(nonce % 256);
        return array;
      },
    },
  });
  Object.defineProperty(globalThis, "crypto", {
    configurable: true,
    value: testCrypto,
  });
}

class TestResizeObserver implements ResizeObserver {
  disconnect(): void {}
  observe(): void {}
  unobserve(): void {}
}

Object.defineProperty(globalThis, "ResizeObserver", {
  configurable: true,
  value: TestResizeObserver,
});

if (typeof Element !== "undefined") {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: () => {},
  });
}

if (typeof navigator !== "undefined") {
  Object.defineProperty(globalThis.navigator, "clipboard", {
    configurable: true,
    value: {
      readText: async () => "",
      writeText: async () => {},
    },
  });
}

afterEach(() => cleanup());

function createMemoryStorage(): Storage {
  const values = new Map<string, string>();
  return {
    get length() {
      return values.size;
    },
    clear() {
      values.clear();
    },
    getItem(key) {
      return values.get(key) ?? null;
    },
    key(index) {
      return [...values.keys()][index] ?? null;
    },
    removeItem(key) {
      values.delete(key);
    },
    setItem(key, value) {
      values.set(key, value);
    },
  };
}
