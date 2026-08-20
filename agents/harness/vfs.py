"""Virtual filesystem tool surface, lifted from DeepAgents and reimplemented
natively (no `deepagents`/LangGraph dependency — see
docs/AMH-SPECIFICATION.md §14.1-§14.2). Backs filesystem-as-context: large
tool results, research notes, and scratchpads live as files referenced by
handle, loaded just-in-time, rather than held in the context window.

V0 backend: local disk, one root directory per agent run (matching the
"local disk" backend option named in §4; a graph-store backend is a
post-V0 concern). Every path is resolved relative to and confined within
the VFS root — no traversal outside it.
"""

from __future__ import annotations

import fnmatch
import os
import re
from dataclasses import dataclass
from pathlib import Path


class PathEscapesRootError(Exception):
    """Raised when a requested path would resolve outside the VFS root —
    the isolation boundary a sub-agent's VFS must never be able to cross."""


@dataclass
class ListEntry:
    path: str
    is_dir: bool
    size: int


class VFS:
    def __init__(self, root: str):
        self.root = Path(root).resolve()
        self.root.mkdir(parents=True, exist_ok=True)

    def _resolve(self, path: str) -> Path:
        candidate = (self.root / path).resolve()
        if candidate != self.root and self.root not in candidate.parents:
            raise PathEscapesRootError(f"{path!r} resolves outside VFS root {self.root}")
        return candidate

    def write_file(self, path: str, content: str) -> None:
        target = self._resolve(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)

    def read_file(self, path: str, offset: int = 0, limit: int | None = None) -> str:
        target = self._resolve(path)
        text = target.read_text()
        lines = text.splitlines(keepends=True)
        if limit is None:
            return "".join(lines[offset:])
        return "".join(lines[offset:offset + limit])

    def edit_file(self, path: str, old: str, new: str) -> None:
        target = self._resolve(path)
        text = target.read_text()
        if old not in text:
            raise ValueError(f"{old!r} not found in {path}")
        target.write_text(text.replace(old, new, 1))

    def ls(self, path: str = ".") -> list[ListEntry]:
        target = self._resolve(path)
        entries = []
        for child in sorted(target.iterdir()):
            entries.append(ListEntry(
                path=str(child.relative_to(self.root)),
                is_dir=child.is_dir(),
                size=child.stat().st_size if child.is_file() else 0,
            ))
        return entries

    def glob(self, pattern: str) -> list[str]:
        matches = []
        for dirpath, _, filenames in os.walk(self.root):
            for filename in filenames:
                rel = str((Path(dirpath) / filename).relative_to(self.root))
                if fnmatch.fnmatch(rel, pattern):
                    matches.append(rel)
        return sorted(matches)

    def grep(self, pattern: str, path: str = ".") -> list[tuple[str, int, str]]:
        """Returns (relative_path, line_number, line_text) for every match."""
        target = self._resolve(path)
        regex = re.compile(pattern)
        hits: list[tuple[str, int, str]] = []
        files = [target] if target.is_file() else [
            Path(dirpath) / f for dirpath, _, files in os.walk(target) for f in files
        ]
        for f in files:
            try:
                for i, line in enumerate(f.read_text().splitlines(), start=1):
                    if regex.search(line):
                        hits.append((str(f.relative_to(self.root)), i, line))
            except UnicodeDecodeError:
                continue
        return hits
