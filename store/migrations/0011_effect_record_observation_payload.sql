-- §9 acceptance invariant #9: "the trajectory presented to a model is
-- reconstructible from durable records/artifacts." Nothing durably
-- captured the actual content an effect observed — MarkObserved's
-- observationRef is a reference (e.g. daemon/extensions passes its own
-- runtime handle), never the evidence itself, and daemon/inference had
-- nowhere to put a real model response, so it passed "" and the response
-- was discarded after use. Nullable, no default: most callers (e.g.
-- daemon/extensions) still have nothing to put here, since their own
-- observation_ref already is a sufficient reference.
ALTER TABLE effect_record ADD COLUMN observation_payload TEXT;
