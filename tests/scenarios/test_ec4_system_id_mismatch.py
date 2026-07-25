"""
EC4: Patroni system ID mismatch self-healing.

Two production scenarios are covered:

  EC4a – Stale /initialize key (Case 1 in checkPatroniSystemID)
    After a dead-cluster-recovery a fresh node briefly bootstraps PG, sets
    /patroni/<app>/initialize to its new system ID, then crashes or yields.
    The surviving cluster members have the OLD system ID.  Any node that
    restarts during this window will crash-loop in Patroni with
    "CRITICAL: system ID mismatch".

    The daemon's checkPatroniSystemID detects that no running peer confirms
    the /initialize value and the primary has a different system ID →
    it updates /initialize to the primary's system ID so crash-looping
    replicas can rejoin without touching their PG data.

  EC4b – Isolated fresh node cannot create a competing epoch
    A completely fresh replacement may temporarily see only itself during
    high node churn. It must not infer that the cluster is new. Bootstrap is
    allowed only after every expected peer is reachable and repeatedly
    confirms empty PostgreSQL and etcd state with the same membership view.

Data-safety note (post-incident hardening):
  The PG data wipe in Case 2 is destructive, so it is now OPT-IN via
  ALLOW_PG_DATA_WIPE (default false) and additionally requires the live
  PRIMARY (not merely any re-cloned replica) to confirm the authoritative
  system ID before any data is deleted. This prevents a stray/empty epoch —
  or a replica cloned from one — from causing surviving nodes to wipe real
  data. Neither scenario in this module relies on deleting PostgreSQL data.

Timing with test overrides (UPDATE_INTERVAL=15s):
  EC4a: inject bad key → kill/restart replica → daemon corrects key in
        ≤2 update cycles (~30s) → Patroni rejoins.  Timeout: 180s.
  EC4b: isolated fresh node remains uninitialized while the original cluster
        is unavailable; the original epoch recovers unchanged.
"""

import subprocess
import time

import docker as docker_sdk
import pytest

from tests.helpers.cluster import ClusterManager
from tests.helpers.mock_api import MockApiClient


# ---------------------------------------------------------------------------
# Module-scoped fixtures (one docker-compose up/down per module)
# ---------------------------------------------------------------------------


@pytest.fixture(scope="module")
def running_cluster(project_dir, compose_cmd, built_image):
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
    subprocess.run(compose_cmd + ["up", "-d"], cwd=project_dir, check=True)
    yield
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)


@pytest.fixture(scope="module")
def cluster(running_cluster):
    manager = ClusterManager(docker_sdk.from_env())
    manager.wait_for_healthy(timeout=300)
    yield manager
    manager.cleanup_dynamic()


@pytest.fixture(scope="module")
def mock_api(cluster):
    return MockApiClient()


# ---------------------------------------------------------------------------
# EC4a – stale /initialize key is corrected by the daemon
# ---------------------------------------------------------------------------


def test_stale_initialize_key_is_healed(cluster: ClusterManager, mock_api: MockApiClient):
    """
    EC4a: Injecting a fake /initialize key causes a restarted replica to
    crash-loop.  The daemon must detect that the primary's system ID differs
    from /initialize and update the key, allowing the replica to rejoin.
    """
    cluster.wait_for_healthy(expected_members=3, timeout=120)
    # Explicitly wait for etcd gRPC to be ready on the node we'll query.
    # wait_for_healthy only checks the Patroni HTTP+SQL path; without this,
    # etcdctl can return DeadlineExceeded during the first few seconds.
    cluster.wait_for_etcd_ready("postgres-node1", timeout=60)

    # Record the real system ID (what /initialize should always contain).
    real_sysid = cluster.etcd_get("postgres-node1", "/patroni/postgres-cluster/initialize")
    assert real_sysid, "initialize key must be set in a healthy cluster"

    # Identify a non-primary replica to restart.
    leader = cluster.get_leader()
    assert leader is not None
    victim = "postgres-node2" if leader != "node-172-20-0-11" else "postgres-node3"

    # --- Inject the fault ---
    # Overwrite /initialize with a fake system ID.  Any replica that (re)starts
    # while this key is wrong will hit "CRITICAL: system ID mismatch".
    cluster.etcd_set("postgres-node1", "/patroni/postgres-cluster/initialize", "FAKE_SYSID_9999999999999")

    # Restart the victim replica so it enters the crash-loop.
    cluster.kill_node(victim)
    time.sleep(3)
    cluster.start_node(victim)

    # --- Expect auto-heal ---
    # The daemon on the surviving nodes will find that no peer confirms the fake
    # /initialize value and that the primary has the real system ID → it updates
    # /initialize back to real_sysid within ≤2 update cycles (~30s).
    # Once the key is correct the restarted node's Patroni succeeds on its next
    # retry and the cluster becomes fully healthy again.
    cluster.wait_for_healthy(expected_members=3, timeout=180)

    healed_sysid = cluster.etcd_get("postgres-node1", "/patroni/postgres-cluster/initialize")
    assert healed_sysid == real_sysid, (
        f"initialize key was not restored: got {healed_sysid!r}, want {real_sysid!r}"
    )

    # All nodes must agree on the same system ID.  Patroni REST API on a node
    # that just restarted may take a few extra seconds to become available even
    # after wait_for_healthy returns, so retry briefly.
    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        deadline = time.time() + 30
        node_sysid = None
        while time.time() < deadline:
            node_sysid = cluster.get_pg_system_id(node)
            if node_sysid:
                break
            time.sleep(2)
        assert node_sysid == real_sysid, (
            f"{node} system ID {node_sysid!r} does not match cluster {real_sysid!r}"
        )


# ---------------------------------------------------------------------------
# EC4b – isolated fresh node cannot bootstrap a competing epoch
# ---------------------------------------------------------------------------


def test_fresh_isolated_node_cannot_bootstrap_competing_epoch(
    cluster: ClusterManager, mock_api: MockApiClient
):
    """
    EC4b: A fresh node that temporarily sees only itself must remain
    uninitialized. Once it is removed and the original nodes return, the
    original PostgreSQL epoch must recover unchanged.
    """
    cluster.wait_for_healthy(expected_members=3, timeout=120)
    cluster.wait_for_etcd_ready("postgres-node1", timeout=60)

    original_sysid = cluster.etcd_get("postgres-node1", "/patroni/postgres-cluster/initialize")
    assert original_sysid, "initialize key must be set"

    # --- Kill all base nodes ---
    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        cluster.kill_node(node)
    time.sleep(5)

    # test-node4 has no volumes and temporarily sees only itself. This is
    # ambiguous (first deployment vs partition/churn), so automatic bootstrap
    # must fail closed.
    mock_api.set_nodes(["172.20.0.13"])
    cluster.spawn_fresh_node("test-node4")

    time.sleep(75)
    node4 = cluster._container("test-node4")
    result = node4.exec_run(
        ["sh", "-c", "test ! -e /var/lib/postgresql/data/global/pg_control"]
    )
    assert result.exit_code == 0, (
        "isolated fresh node created PostgreSQL data; automatic bootstrap "
        f"must remain blocked: {result.output.decode(errors='replace')}"
    )
    assert cluster.patroni_status("test-node4") is None

    cluster.remove_fresh_node("test-node4")
    mock_api.set_nodes(["172.20.0.10", "172.20.0.11", "172.20.0.12"])
    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        cluster.start_node(node)

    cluster.wait_for_healthy(expected_members=3, timeout=300)

    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        node_sysid = cluster.get_pg_system_id(node)
        assert node_sysid == original_sysid, (
            f"{node} system ID changed from preserved epoch {original_sysid!r} "
            f"to {node_sysid!r}"
        )

    rows = cluster.exec_sql("SELECT 1")
    assert rows == [(1,)]
