"""The integration harness must not mistake cached membership for recovery."""

from contextlib import nullcontext

import pytest

from tests.helpers import cluster as helpers


@pytest.fixture(autouse=True)
def reset_mock_api():
    # Unit tests require neither a mock API nor running Docker containers.
    yield


class Cursor:
    def __init__(self, writable):
        self.writable = writable

    def execute(self, query):
        assert "pg_is_in_recovery()" in query

    def fetchone(self):
        return (False, "off") if self.writable else (True, "on")


class Connection:
    def __init__(self, writable):
        self.writable = writable

    def cursor(self):
        return nullcontext(Cursor(self.writable))


@pytest.mark.parametrize("writable,live_count,healthy", [(True, 3, True), (False, 3, False), (True, 1, False)])
def test_readiness_requires_writable_primary_and_direct_live_members(monkeypatch, writable, live_count, healthy):
    manager = helpers.ClusterManager(None)
    nodes = list(helpers.BASE_NODES)
    names = ["node-" + helpers.BASE_NODES[node].ip.replace(".", "-") for node in nodes]
    cached = {"members": [
        {"name": name, "role": "leader" if i == 0 else "replica", "state": "running"}
        for i, name in enumerate(names)
    ]}
    monkeypatch.setattr(manager, "cluster_status", lambda: cached)
    monkeypatch.setattr(manager, "patroni_status", lambda node: (
        {"state": "running", "patroni": {"name": names[nodes.index(node)]}}
        if nodes.index(node) < live_count else None
    ))
    monkeypatch.setattr(helpers.psycopg2, "connect", lambda **kwargs: nullcontext(Connection(writable)))
    now = [0]
    monkeypatch.setattr(helpers.time, "time", lambda: now[0])
    monkeypatch.setattr(helpers.time, "sleep", lambda delay: now.__setitem__(0, now[0] + delay))
    if healthy:
        assert manager.wait_for_healthy(timeout=1)
    else:
        with pytest.raises(TimeoutError):
            manager.wait_for_healthy(timeout=1)
