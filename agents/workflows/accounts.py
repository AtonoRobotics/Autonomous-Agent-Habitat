"""Account HTTP client — read-only, deliberately, for the same reason as
extensions.py: creating an account, storing/rotating its credential
("authenticate an account"), and revoking are all operator-only on
daemon/api's control-plane routes. Secret material and external identity
are exactly what §1 decision 9 reserves from autonomous agent action.

This module never sees, requests, or could transmit a secret — the
underlying daemon/credentials.Account type it deserializes into never
carries one, and no function here accepts anything secret-shaped.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request


class AccountError(Exception):
    pass


def list_accounts(daemon_api_base_url: str, agent_token: str) -> list[dict]:
    url = f"{daemon_api_base_url}/v1/accounts"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise AccountError(payload.get("error", f"HTTP {e.code}")) from e


def get_account(daemon_api_base_url: str, agent_token: str, account_id: str) -> dict:
    url = f"{daemon_api_base_url}/v1/accounts/{account_id}"
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {agent_token}"})
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as e:
        payload = json.loads(e.read())
        raise AccountError(payload.get("error", f"HTTP {e.code}")) from e
