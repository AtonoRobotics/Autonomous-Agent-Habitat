"""write_todos planning tool. Opt-in upstream in DeepAgents v0.7 (§14.2);
AMH enables it by default and persists the list to the durable log so plans
survive restarts, per docs/AMH-SPECIFICATION.md §14.1/Artifact G
(`planning: { write_todos: true }` in agent.manifest.yaml).
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass
from enum import Enum

from .vfs import VFS

TODOS_PATH = "todos.json"


class TodoStatus(str, Enum):
    PENDING = "pending"
    IN_PROGRESS = "in_progress"
    COMPLETED = "completed"


@dataclass
class Todo:
    id: str
    text: str
    status: TodoStatus = TodoStatus.PENDING


class TodoList:
    """A structured task list scoped to one agent's VFS. Persisting to a
    VFS file (rather than only in-memory) is what lets a plan survive a
    context-window compaction or a process restart — the same durability
    property pursue_goal gets from DBOS, applied to planning state."""

    def __init__(self, vfs: VFS):
        self.vfs = vfs
        self._todos: list[Todo] = self._load()

    def _load(self) -> list[Todo]:
        try:
            raw = json.loads(self.vfs.read_file(TODOS_PATH))
        except FileNotFoundError:
            return []
        return [Todo(id=t["id"], text=t["text"], status=TodoStatus(t["status"])) for t in raw]

    def _save(self) -> None:
        self.vfs.write_file(TODOS_PATH, json.dumps([asdict(t) for t in self._todos], default=str))

    def write_todos(self, items: list[str]) -> list[Todo]:
        """Replaces the whole list — matching DeepAgents' write_todos
        semantics (the model re-emits its full current plan each call)."""
        self._todos = [Todo(id=str(i), text=text) for i, text in enumerate(items)]
        self._save()
        return list(self._todos)

    def set_status(self, todo_id: str, status: TodoStatus) -> None:
        for t in self._todos:
            if t.id == todo_id:
                t.status = status
                self._save()
                return
        raise KeyError(f"no todo with id {todo_id!r}")

    def all(self) -> list[Todo]:
        return list(self._todos)
