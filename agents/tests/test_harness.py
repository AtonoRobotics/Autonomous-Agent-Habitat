from __future__ import annotations

import pytest

from harness.planning import TodoList, TodoStatus
from harness.subagent import CONDENSED_RESULT_KEYS, condense, spawn
from harness.vfs import PathEscapesRootError, VFS


def test_vfs_write_read_roundtrip(tmp_path):
    vfs = VFS(str(tmp_path))
    vfs.write_file("notes/scratch.txt", "hello world")
    assert vfs.read_file("notes/scratch.txt") == "hello world"


def test_vfs_confines_paths_to_root(tmp_path):
    vfs = VFS(str(tmp_path / "root"))
    with pytest.raises(PathEscapesRootError):
        vfs._resolve("../escape.txt")
    with pytest.raises(PathEscapesRootError):
        vfs._resolve("/etc/passwd")


def test_vfs_glob_and_grep(tmp_path):
    vfs = VFS(str(tmp_path))
    vfs.write_file("a.log", "error: disk full\nok: retrying\n")
    vfs.write_file("b.log", "ok: nothing to see\n")
    vfs.write_file("c.txt", "irrelevant")

    assert sorted(vfs.glob("*.log")) == ["a.log", "b.log"]

    hits = vfs.grep("error:")
    assert len(hits) == 1
    assert hits[0][0] == "a.log"
    assert hits[0][1] == 1


def test_write_todos_persists_across_reload(tmp_path):
    vfs = VFS(str(tmp_path))
    todos = TodoList(vfs)
    written = todos.write_todos(["poll temperature", "open vent if hot"])
    assert [t.text for t in written] == ["poll temperature", "open vent if hot"]
    assert all(t.status == TodoStatus.PENDING for t in written)

    todos.set_status("0", TodoStatus.IN_PROGRESS)

    # A fresh TodoList over the same VFS root must see the persisted state —
    # this is what lets a plan survive compaction/restart.
    reloaded = TodoList(vfs)
    assert reloaded.all()[0].status == TodoStatus.IN_PROGRESS
    assert reloaded.all()[1].status == TodoStatus.PENDING


def test_subagent_gets_isolated_vfs_unreachable_from_parent(tmp_path):
    workspace_root = str(tmp_path / "run-1")
    parent_vfs = VFS(str(tmp_path / "run-1" / "parent"))
    parent_vfs.write_file("secret.txt", "parent-only data")

    handle = spawn(workspace_root, task_id="task-1")
    handle.vfs.write_file("child_note.txt", "child data")

    # The child's VFS root is isolated: it cannot see the parent's files...
    assert handle.vfs.glob("*.txt") == ["child_note.txt"]
    # ...and the parent's own VFS instance (confined to its own root) has
    # no view into the child's sibling root at all — not even by wildcard.
    assert parent_vfs.glob("*.txt") == ["secret.txt"]
    with pytest.raises(PathEscapesRootError):
        parent_vfs._resolve("../subagents/task-1/child_note.txt")


def test_condense_strips_to_result_only_contract():
    raw = {
        "task_id": "t1",
        "status": "done",
        "summary": "completed the thing",
        "full_reasoning_trace": ["step 1", "step 2", "internal thought"],
        "raw_tool_outputs": {"huge": "blob" * 1000},
    }
    condensed = condense(raw)
    assert set(condensed.keys()) == CONDENSED_RESULT_KEYS
    assert "full_reasoning_trace" not in condensed
    assert "raw_tool_outputs" not in condensed
