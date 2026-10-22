"""HTTP client for the ingress agent: pushes the live credential set, reads traffic stats.

Failed pushes raise a typed ``AgentUnavailable`` (or a subclass) whose ``outcome`` names what went
wrong and whose ``status`` is the HTTP status, if any. Messages never carry payloads or credentials.
"""
import requests

PUT_TIMEOUT = 10
STATS_TIMEOUT = 2  # the admin page waits on it


class AgentUnavailable(Exception):
    """The push did not take effect. Base class: the agent could not be reached at all."""

    outcome = "unreachable"

    def __init__(self, message: str = "", status: "int | None" = None):
        super().__init__(message)
        self.status = status


class AgentUnauthorized(AgentUnavailable):
    """401/403: the bearer token is wrong (AGENT_TOKEN differs between the two sides)."""

    outcome = "unauthorized"


class AgentConflict(AgentUnavailable):
    """409: the agent refused an empty set that was not confirmed."""

    outcome = "conflict"


class AgentRejected(AgentUnavailable):
    """Any other non-2xx, typically 503: xray refused the generated config or did not restart."""

    outcome = "rejected"


def _error_for(status: int) -> AgentUnavailable:
    msg = f"agent answered {status}"
    if status in (401, 403):
        return AgentUnauthorized(msg, status=status)
    if status == 409:
        return AgentConflict(msg, status=status)
    return AgentRejected(msg, status=status)


class AgentClient:
    def __init__(self, base_url: str, token: str):
        self.base_url = base_url.rstrip("/")
        self._headers = {"Authorization": f"Bearer {token}"}

    def put_credentials(self, creds: list, confirm_empty: bool = False) -> None:
        kwargs = {}
        if confirm_empty:
            kwargs["params"] = {"confirm_empty": "true"}
        try:
            resp = requests.put(
                f"{self.base_url}/v1/credentials",
                headers=self._headers,
                json={"credentials": creds},
                timeout=PUT_TIMEOUT,
                **kwargs,
            )
        except requests.RequestException as e:
            raise AgentUnavailable(type(e).__name__) from e
        if not 200 <= resp.status_code < 300:
            raise _error_for(resp.status_code)

    def get_stats(self) -> dict:
        """Per-device traffic keyed by opaque id; ``{}`` when the agent is unavailable."""
        try:
            resp = requests.get(f"{self.base_url}/v1/stats", headers=self._headers, timeout=STATS_TIMEOUT)
            if not 200 <= resp.status_code < 300:
                return {}
            data = resp.json()
        except (requests.RequestException, ValueError):
            return {}
        return data if isinstance(data, dict) else {}
