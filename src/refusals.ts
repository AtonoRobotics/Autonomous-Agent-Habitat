/**
 * Typed refusals. A refusal returns a code the field language map can render and
 * a test can assert — silence is not a refusal. Codes are added as the subsystem
 * that raises them lands, and never quietly renamed.
 */
export const REFUSAL_CODES = [
  "SURFACE_VIOLATION",
  "ONE_GOAL",
  "NO_OPEN_RUN",
  "DENY_IS_TERMINAL",
  "SURPRISE_GRADUATION",
  "GRANT_BOUNDS",
  "GRANT_RATE",
  "GRANT_EXPIRED",
  "PACK_UNSIGNED",
  "PACK_INCOMPLETE",
  "PACK_INVALID",
  "WAKE_LOG_MISMATCH",
  "TALKING_PASS",
  "NOT_ORCHESTRATOR",
  "MEMORY_NOT_FACT",
  "MEMORY_UNLABELED",
  "MODEL_CANNOT_VERIFY",
  "EVIDENCE_IMMUTABLE",
  "ADAPTER_UNBOUND",
  "ADAPTER_CREDENTIALS_MISSING",
  "ADAPTER_NOT_ALLOWED",
  "CONNECTOR_UNBOUND",
  "CONNECTOR_CREDENTIALS_MISSING",
  "CONNECTOR_UNREACHABLE",
  "CONNECTOR_REJECTED",
  "HABITAT_CANNOT_PROMOTE",
] as const;

export type RefusalCode = (typeof REFUSAL_CODES)[number];

/** Every refusal carries its code. Callers match on `code`, never on the message. */
export class Refusal extends Error {
  readonly code: RefusalCode;

  constructor(code: RefusalCode, message: string) {
    super(message);
    this.name = "Refusal";
    this.code = code;
  }
}

export function isRefusal(error: unknown, code?: RefusalCode): error is Refusal {
  if (!(error instanceof Refusal)) return false;
  return code === undefined || error.code === code;
}
