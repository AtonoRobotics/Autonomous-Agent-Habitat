/**
 * The wake is the single entry point to the habitat. The set is closed: if an
 * event is not one of these, it does not move the team.
 */
export const WAKE_KINDS = [
  "field_start",
  "field_ask",
  "field_continue",
  "card_decide",
  "worker_done",
  "worker_failed",
  "kill",
  "deadline",
  "connector",
  "routine",
  "mail",
  "architect_message",
] as const;

export type WakeKind = (typeof WAKE_KINDS)[number];

const WAKE_KIND_SET: ReadonlySet<string> = new Set(WAKE_KINDS);

export function isWakeKind(value: string): value is WakeKind {
  return WAKE_KIND_SET.has(value);
}

/**
 * A wake cuts in only when it is a kill; everything else queues behind the
 * in-flight pass.
 */
export function cutsInLine(kind: WakeKind): boolean {
  return kind === "kill";
}
