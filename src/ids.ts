import { randomBytes } from "node:crypto";

/** Prefixes are closed: an id whose prefix is not here is not one of ours. */
export const ID_PREFIXES = [
  "card",
  "journey",
  "rec",
  "act",
  "run",
  "worker",
  "grant",
  "agent",
  "ftok",
  "mail",
  "ev",
  "asrt",
  "party",
  "out",
  "notice",
  "log",
  "recall",
  "packet",
] as const;

export type IdPrefix = (typeof ID_PREFIXES)[number];
export type Id<P extends IdPrefix = IdPrefix> = `${P}_${string}`;

const ID_BYTES = 12; // 24 hex characters
const ID_PATTERN = /^([a-z]+)_([0-9a-f]{24})$/;

export function newId<P extends IdPrefix>(prefix: P): Id<P> {
  return `${prefix}_${randomBytes(ID_BYTES).toString("hex")}`;
}

export function isId(value: string, prefix?: IdPrefix): boolean {
  const match = ID_PATTERN.exec(value);
  if (match === null) return false;
  const found = match[1] as IdPrefix;
  if (prefix !== undefined) return found === prefix;
  return (ID_PREFIXES as readonly string[]).includes(found);
}
