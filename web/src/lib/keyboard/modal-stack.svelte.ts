export interface KeySpec {
  key: string;
  ctrlOrMeta?: boolean;
  shift?: boolean;
  alt?: boolean;
}

export interface ModalFrameAction {
  id: string;
  label: string;
  binding: KeySpec | KeySpec[] | null;
  priority?: number;
  when?: (context: unknown) => boolean;
  handler: (context: unknown) => void;
}

export interface ModalFrame {
  frameId: string;
  actions: ModalFrameAction[];
}

// Internal frames carry a unique token so cleanup can identify the exact
// frame that was pushed even if the rune proxy wraps the stored object,
// which breaks reference identity for `indexOf`/`===` comparisons.
interface InternalFrame extends ModalFrame {
  __token: symbol;
}

let stack = $state<InternalFrame[]>([]);

export function pushModalFrame(
  frameId: string,
  actions: ModalFrameAction[],
): () => void {
  const token = Symbol(frameId);
  stack = [...stack, { frameId, actions: [...actions], __token: token }];
  return () => {
    // Remove by per-push token, not frameId. Two pushes with the same frameId
    // produce distinct tokens; popping one must not remove the other.
    const next = stack.filter((f) => f.__token !== token);
    if (next.length !== stack.length) {
      stack = next;
    }
  };
}

export function getTopFrame(): ModalFrame | null {
  if (stack.length === 0) return null;
  const top = stack[stack.length - 1]!;
  return { frameId: top.frameId, actions: [...top.actions] };
}

export function getStackDepth(): number {
  return stack.length;
}

export function getStack(): ModalFrame[] {
  return stack.map((f) => ({
    frameId: f.frameId,
    actions: [...f.actions],
  }));
}

export function resetModalStack(): void {
  stack = [];
}
