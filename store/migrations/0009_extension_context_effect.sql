-- §15 acceptance invariant #3 / §5.1 "Cordis temporal composability":
-- "Every core-mediated software effect installed by an extension SHALL
-- register its disposer at the time the effect is created. Effects
-- SHALL be disposed in reverse registration order." Core cannot
-- literally execute an arbitrary extension's disposer itself — only
-- the extension knows how to actually undo "tool registration" or
-- "timer stop," the same reason §4 says core "SHALL NOT construct an
-- inverse" for external effects. What core CAN honestly enforce is
-- order and completeness: an active extension self-reports each
-- core-mediated effect it creates, then self-reports disposing each
-- one, and daemon/extensions.Dispose refuses (fails closed) unless
-- every registered effect for that instance has actually been disposed
-- in exact reverse order — see daemon/extensions/context_effect.go.
--
-- kind/ref are free-form and opaque to core (a "tool"/"event"/"timer"/
-- "route"/"service"/"child-extension" registration per §5.1's examples,
-- or any future one) — this table doesn't hardcode the taxonomy, the
-- same domain-neutral posture every other core primitive in this
-- schema already takes (daemon/operations' effect_type, daemon/policy's
-- action payload).
--
-- One row per registered effect, sequence monotonic per (extension_id,
-- extension_version) instance — assigned by the application, not a
-- DB-generated identity column, so "the most recently registered
-- still-outstanding effect" can be queried directly rather than
-- inferred from insertion order alone.
CREATE TABLE extension_context_effect (
  id TEXT PRIMARY KEY,
  extension_id TEXT NOT NULL,
  extension_version TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  kind TEXT NOT NULL,
  ref TEXT NOT NULL,
  registered_at TEXT NOT NULL DEFAULT iso8601_now(),
  disposed_at TEXT,
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version),
  UNIQUE (extension_id, extension_version, sequence)
);
CREATE INDEX idx_extension_context_effect_instance ON extension_context_effect(extension_id, extension_version, sequence);
