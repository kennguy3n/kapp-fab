import { api } from "./api";
import { registerReplayHandler, type QueuedMutation } from "./offlineQueue";

// Mutation discriminator for POS finalizes in the shared offline queue.
// Entries of this type are stored alongside any other queued mutations
// but replayed by the handler registered below.
export const POS_MUTATION_TYPE = "pos.finalize";

/** Replay body for a queued POS finalize. The queue entry's `id` is the
 *  idempotency key reused on retry so duplicates collapse server-side. */
export interface POSFinalizePayload {
  posInvoiceId: string;
  total: number;
}

/**
 * Replay a single queued POS finalize. Depends only on the singleton
 * `api` client and the mutation payload — NOT on any React state — which
 * is what lets it be registered for the app's whole lifetime instead of
 * only while POSPage is mounted.
 */
export const posFinalizeReplay = async (
  mutation: QueuedMutation,
): Promise<void> => {
  const payload = mutation.payload as POSFinalizePayload;
  await api.finalizePOSInvoice(payload.posInvoiceId, mutation.id);
};

let registered = false;

/**
 * Register the POS replay handler exactly once, at app startup, so the
 * shell-level `drainAll()` can replay queued POS finalizes on reconnect
 * even if POSPage was never mounted this session. Previously the handler
 * was registered inside POSPage's effect and torn down on unmount, so a
 * reconnect while on any other route skipped POS entries until the user
 * next opened the page — this closes that gap. Idempotent: safe to call
 * more than once (e.g. under React StrictMode double-invoke).
 */
export function registerPOSReplay(): void {
  if (registered) return;
  registered = true;
  registerReplayHandler(POS_MUTATION_TYPE, posFinalizeReplay);
}
