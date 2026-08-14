import { beforeEach, describe, expect, it } from "vitest";

import {
  pushModalFrame,
  getTopFrame,
  getStackDepth,
  resetModalStack,
} from "./modal-stack.svelte";

describe("modal stack", () => {
  beforeEach(() => resetModalStack());

  it("starts empty", () => {
    expect(getStackDepth()).toBe(0);
    expect(getTopFrame()).toBeNull();
  });

  it("push then pop", () => {
    const pop = pushModalFrame("palette", []);
    expect(getStackDepth()).toBe(1);
    expect(getTopFrame()?.frameId).toBe("palette");
    pop();
    expect(getStackDepth()).toBe(0);
  });

  it("nested frames return topmost first", () => {
    const popA = pushModalFrame("a", []);
    pushModalFrame("b", []);
    expect(getTopFrame()?.frameId).toBe("b");
    popA(); // out-of-order pop is allowed; the frame is removed wherever it sits
    expect(getStackDepth()).toBe(1);
    expect(getTopFrame()?.frameId).toBe("b");
  });

  it("duplicate frameId pushes are independent (token identity)", () => {
    const popFirst = pushModalFrame("dup", []);
    pushModalFrame("dup", []);
    expect(getStackDepth()).toBe(2);
    popFirst();
    expect(getStackDepth()).toBe(1);
    // The second "dup" frame survives because pop is by frame-object identity.
    expect(getTopFrame()?.frameId).toBe("dup");
  });

  it("pop is idempotent and removes only the captured frame", () => {
    const pop = pushModalFrame("only", []);
    pop();
    expect(getStackDepth()).toBe(0);
    // Calling the cleanup again must not corrupt later pushes.
    pop();
    pushModalFrame("after", []);
    expect(getStackDepth()).toBe(1);
    expect(getTopFrame()?.frameId).toBe("after");
  });
});
