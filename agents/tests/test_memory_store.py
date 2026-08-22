"""Tests for memory/store.py — the episodic/semantic/procedural/entity
projections (docs/AMH-SPECIFICATION.md §8). Pure SQLite-level tests against
a real migrated db_path (see conftest.py), no daemon needed — matches
test_context.py's convention for testing plain accessor logic.
"""

from __future__ import annotations

from memory import store


def test_write_and_read_episode(db_path):
    store.write_episode(db_path, run_id="run-1", actor="agent-1", payload={"note": "started"}, salience=0.8)
    store.write_episode(db_path, run_id="run-1", actor="agent-1", payload={"note": "finished"}, salience=0.3)
    store.write_episode(db_path, run_id="run-2", actor="agent-2", payload={"note": "other run"})

    episodes = store.recent_episodes(db_path, run_id="run-1")
    assert len(episodes) == 2
    assert episodes[0].payload == {"note": "finished"}  # most recent first
    assert episodes[1].payload == {"note": "started"}
    assert episodes[1].salience == 0.8


def test_current_claims_returns_only_unsuperseded_facts(db_path):
    c1 = store.write_claim(db_path, subj="vent-1", pred="has_state", obj="open", confidence=0.9)
    claims = store.current_claims(db_path, subj="vent-1")
    assert [c.obj for c in claims] == ["open"]

    c2 = store.write_claim(db_path, subj="vent-1", pred="has_state", obj="closed", confidence=0.95, supersedes=c1)
    claims = store.current_claims(db_path, subj="vent-1")
    assert [c.obj for c in claims] == ["closed"]
    assert claims[0].id == c2
    assert claims[0].supersedes == c1

    # The superseded claim is gone from "current" but still exists (bi-temporal history preserved).
    with store.connect(db_path) as conn:
        row = conn.execute("SELECT invalid_at, expired_at FROM fact WHERE id = ?", (c1,)).fetchone()
    assert row[0] is not None and row[1] is not None


def test_contradicts_is_a_pure_link_not_an_invalidation(db_path):
    c1 = store.write_claim(db_path, subj="sensor-1", pred="reads", obj="22C")
    c2 = store.write_claim(db_path, subj="sensor-1", pred="reads", obj="19C", contradicts=c1)

    claims = store.current_claims(db_path, subj="sensor-1")
    assert {c.obj for c in claims} == {"22C", "19C"}  # both still current
    assert next(c for c in claims if c.id == c2).contradicts == c1


def test_current_claims_respects_as_of_valid_time(db_path):
    claim_id = store.write_claim(db_path, subj="x", pred="p", obj="o", valid_at="2020-01-01T00:00:00.000Z")
    assert store.current_claims(db_path, subj="x", as_of="2019-01-01T00:00:00.000Z") == []
    assert len(store.current_claims(db_path, subj="x", as_of="2021-01-01T00:00:00.000Z")) == 1


def test_write_skill_and_playbook_and_promoted_filter(db_path):
    store.write_skill(db_path, "skill-1", "v1", name="open vent", promoted=True, kind="skill")
    store.write_skill(db_path, "skill-2", "v1", name="not promoted yet", promoted=False, kind="skill")
    store.write_skill(db_path, "pb-1", "v1", name="morning routine", promoted=True, kind="playbook")

    all_promoted = {s.name for s in store.promoted_skills(db_path)}
    assert all_promoted == {"open vent", "morning routine"}

    only_playbooks = store.promoted_skills(db_path, kind="playbook")
    assert [s.name for s in only_playbooks] == ["morning routine"]

    fetched = store.get_skill(db_path, "skill-1", "v1")
    assert fetched.name == "open vent"
    assert fetched.promoted is True


def test_write_skill_rejects_invalid_kind(db_path):
    import pytest

    with pytest.raises(ValueError):
        store.write_skill(db_path, "s", "v1", kind="bogus")


def test_write_skill_upserts_on_conflict(db_path):
    store.write_skill(db_path, "s", "v1", name="first", eval_score=0.5)
    store.write_skill(db_path, "s", "v1", name="updated", eval_score=0.9)
    fetched = store.get_skill(db_path, "s", "v1")
    assert fetched.name == "updated"
    assert fetched.eval_score == 0.9


def test_entity_resolve_and_profile_assembles_claims_from_all_aliases(db_path):
    store.upsert_entity(db_path, "ent-1", kind="device", canonical_name="vent-1", aliases=["greenhouse vent", "the vent"])

    assert store.resolve_entity(db_path, "the vent") == "ent-1"
    assert store.resolve_entity(db_path, "VENT-1") == "ent-1"  # case-insensitive
    assert store.resolve_entity(db_path, "no such thing") is None

    store.write_claim(db_path, subj="vent-1", pred="has_state", obj="open")
    store.write_claim(db_path, subj="greenhouse vent", pred="located_in", obj="zone-a")
    store.write_claim(db_path, subj="unrelated-thing", pred="has_state", obj="off")

    profile = store.entity_profile(db_path, "ent-1")
    assert profile.canonical_name == "vent-1"
    assert set(profile.aliases) == {"greenhouse vent", "the vent"}
    predicates = {(c.pred, c.obj) for c in profile.claims}
    assert predicates == {("has_state", "open"), ("located_in", "zone-a")}


def test_entity_profile_for_unknown_entity_returns_none(db_path):
    assert store.entity_profile(db_path, "does-not-exist") is None


def test_entity_names_returns_canonical_plus_aliases(db_path):
    store.upsert_entity(db_path, "ent-1", kind="device", canonical_name="vent-1", aliases=["the vent"])
    assert set(store.entity_names(db_path, "ent-1")) == {"vent-1", "the vent"}
    assert store.entity_names(db_path, "nope") == []


def test_write_chunk_and_embedding_roundtrip_with_required_metadata(db_path):
    store.write_chunk(db_path, "chunk-1", "doc-1", "the greenhouse vent is stuck open")
    store.write_embedding(
        db_path, "chunk-1", "the greenhouse vent is stuck open",
        embedding=[0.1, 0.2, 0.3], embedding_identity="test-embed", model_version="v1",
    )
    with store.connect(db_path) as conn:
        row = conn.execute(
            "SELECT dimension, model_version, chunk_version, source_digest, acl FROM chunk_embedding WHERE chunk_id = ?",
            ("chunk-1",),
        ).fetchone()
    assert row[0] == 3
    assert row[1] == "v1"
    assert row[2] == 1
    assert len(row[3]) == 64  # sha256 hex digest
    assert row[4] == "default"
