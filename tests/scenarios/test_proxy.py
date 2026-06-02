"""
Proxy: connection routing to current Patroni primary.

Each node runs a TCP proxy on container port 5433 that forwards every
incoming connection to whichever node Patroni currently reports as primary
(by polling /primary on each node's Patroni REST API).

These tests verify that:
  1. Writes via the proxy succeed regardless of which node we connect to.
  2. When the current primary is killed, the proxy starts routing to the
     newly-elected primary within a reasonable time.
"""

import time

import psycopg2
import pytest

from tests.helpers.cluster import BASE_NODES


PROXY_PORT = 5433


def _connect_via_proxy(node_ip: str, password: str):
    return psycopg2.connect(
        host=node_ip,
        port=PROXY_PORT,
        dbname="postgres",
        user="postgres",
        password=password,
        connect_timeout=10,
        sslmode="prefer",
    )


def _wait_for_proxy_ready(cluster, node_ip: str, password: str, timeout: int = 60) -> None:
    """Poll the proxy until it can dispatch a connection (primary discovered)."""
    deadline = time.time() + timeout
    last_err: Exception | None = None
    while time.time() < deadline:
        try:
            conn = _connect_via_proxy(node_ip, password)
            conn.close()
            return
        except Exception as exc:  # noqa: BLE001
            last_err = exc
            time.sleep(2)
    raise AssertionError(f"proxy on {node_ip} never became ready: {last_err}")


def test_proxy_accepts_writes_via_replica(cluster, mock_api, pg_password):
    """Connecting to the proxy on a replica should still allow writes."""
    cluster.wait_for_healthy(expected_members=3, timeout=180)

    leader = cluster.get_leader()
    assert leader, "no leader found"

    # Pick a non-leader to demonstrate write-routing
    replica_node = next(
        n for n in ("postgres-node1", "postgres-node2", "postgres-node3") if n != leader
    )
    replica_ip = cluster._config_for(replica_node).ip

    _wait_for_proxy_ready(cluster, replica_ip, pg_password)

    with _connect_via_proxy(replica_ip, pg_password) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            cur.execute("DROP TABLE IF EXISTS proxy_write_test")
            cur.execute("CREATE TABLE proxy_write_test (id SERIAL PRIMARY KEY, val TEXT)")
            cur.execute("INSERT INTO proxy_write_test (val) VALUES ('via-proxy')")
            cur.execute("SELECT val FROM proxy_write_test WHERE id = 1")
            assert cur.fetchone() == ("via-proxy",)


def test_proxy_reroutes_after_primary_kill(cluster, mock_api, pg_password):
    """After killing the primary, the proxy should route to the new primary."""
    cluster.wait_for_healthy(expected_members=3, timeout=180)
    old_leader = cluster.get_leader()
    assert old_leader, "no leader found"

    # Use a replica's proxy so we are not killing the node we are connecting to.
    # cluster.get_leader() returns the Patroni name (e.g. node-172-20-0-12);
    # map it to the container key (postgres-nodeN) the kill helpers expect.
    leader_ip = old_leader.replace("node-", "").replace("-", ".")
    leader_container_key = next(
        (k for k, cfg in BASE_NODES.items() if cfg.ip == leader_ip), None
    )
    assert leader_container_key, f"could not map leader {old_leader} to container"

    replica_node = next(
        n for n in ("postgres-node1", "postgres-node2", "postgres-node3") if n != leader_container_key
    )
    replica_ip = cluster._config_for(replica_node).ip

    _wait_for_proxy_ready(cluster, replica_ip, pg_password)

    # Sanity check: write before kill
    with _connect_via_proxy(replica_ip, pg_password) as conn:
        conn.autocommit = True
        with conn.cursor() as cur:
            cur.execute("DROP TABLE IF EXISTS proxy_failover_test")
            cur.execute("CREATE TABLE proxy_failover_test (id SERIAL PRIMARY KEY, marker TEXT)")
            cur.execute("INSERT INTO proxy_failover_test (marker) VALUES ('pre-kill')")

    cluster.kill_node(leader_container_key)
    cluster.wait_for_healthy(expected_members=2, timeout=120)

    # Allow proxy to refresh its primary cache
    deadline = time.time() + 60
    while time.time() < deadline:
        try:
            with _connect_via_proxy(replica_ip, pg_password) as conn:
                conn.autocommit = True
                with conn.cursor() as cur:
                    cur.execute("INSERT INTO proxy_failover_test (marker) VALUES ('post-kill')")
                    cur.execute("SELECT marker FROM proxy_failover_test ORDER BY id")
                    rows = [r[0] for r in cur.fetchall()]
                    assert "pre-kill" in rows and "post-kill" in rows
            cluster.start_node(leader_container_key)
            return
        except Exception:
            time.sleep(3)

    cluster.start_node(leader_container_key)
    pytest.fail("proxy did not reroute writes within 60s after primary kill")
