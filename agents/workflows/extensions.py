"""Extension registry HTTP client — read-only, deliberately. Installing,
activating, quiescing, or disposing an extension is operator-only on the
Go daemon's control-plane API (daemon/api's RBAC table): running new code
with daemon-level reach is exactly the class of action §1 decision 9
("agents propose; deterministic services commit") reserves from
autonomous agent action, unlike a computer's Create/Destroy (see
computers.py), which is a verified inverse pair an agent may perform
itself.

This module has no function that mutates the registry and no function
that could plausibly accept an operator credential — same structural
security posture as approval.py and safetycase.py, just with a stricter
result here: agents get read access only, full stop. An agent that needs
a harness/connector/model-provider extension installed has to ask an
operator; this module gives it the read side of that conversation (what's
already active, what capability a given extension provides) but no path
to act on it alone.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request


class ExtensionError(Exception):
    pass


def list_extensions(daemon_api_base_url: str, agent_token: str) -> list[dict]:
    url = f"{daemon_api_base_url}/v1/extensions"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise ExtensionError(payload.get("error", f"HTTP {e.code}")) from e


def get_extension(daemon_api_base_url: str, agent_token: str, extension_id: str, version: str) -> dict:
    params = urllib.parse.urlencode({"id": extension_id, "version": version})
    url = f"{daemon_api_base_url}/v1/extensions/get?{params}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise ExtensionError(payload.get("error", f"HTTP {e.code}")) from e
