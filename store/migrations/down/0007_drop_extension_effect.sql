-- Reverses store/migrations/0007_drop_extension_effect.sql — recreates
-- extension_effect exactly as 0001_init.sql defined it. Any effect
-- history recorded through effect_record after 0007 applied is not
-- migrated back into this table; rolling back this far means accepting
-- that gap, the same posture every other down-migration in this
-- directory takes toward data it cannot losslessly reconstruct.
CREATE TABLE extension_effect (
  id TEXT PRIMARY KEY,
  extension_id TEXT NOT NULL,
  extension_version TEXT NOT NULL,
  effect_type TEXT NOT NULL CHECK(effect_type IN ('activate','dispose')),
  forward_payload JSON NOT NULL,
  inverse_payload JSON,
  outcome TEXT CHECK(outcome IN ('success','failed','rolled_back')) NOT NULL DEFAULT 'success',
  created_at TEXT NOT NULL DEFAULT iso8601_now(),
  FOREIGN KEY (extension_id, extension_version) REFERENCES extension(id, version)
);
CREATE INDEX idx_extension_effect_lookup ON extension_effect(extension_id, extension_version, effect_type);
