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

  EC4b – Node with wrong-epoch PG data (Case 2 in checkPatroniSystemID)
    A node is replaced and bootstraps a fresh PG cluster (or restores from a
    snapshot of a different cluster), giving it a system ID that doesn't
    match /initialize.  The daemon detects that a healthy peer confirms the
    correct system ID → it wipes local PG data and restarts Patroni, which
    will pg_basebackup from the primary.

    This scenario is reproduced by spawning a completely fresh test-node4 as
    the sole cluster member (it gets a new system ID and owns /initialize),
    then restarting the base nodes that still carry the OLD PG data.

Root cause of the original production failure (why auto-heal was silent):
  - checkPatroniSystemID only accepted role == "primary" (not "master") and
    required the PRIMARY to be reachable from the failing node.
  - When the primary was on a partitioned segment, no primary was found →
    early return → no healing.

Fix: a running REPLICA with the correct system ID is sufficient evidence
that the cluster is healthy.  We no longer require the primary itself.

Timing with test overrides (UPDATE_INTERVAL=15s):
  EC4a: inject bad key → kill/restart replica → daemon corrects key in
        ≤2 update cycles (~30s) → Patroni rejoins.  Timeout: 180s.
  EC4b: fresh node bootstraps (~90s) → base nodes restart → daemon detects
        mismatch, wipes PG, pg_basebackup completes.  Timeout: 480s.
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

    # All nodes must agree on the same system ID.
    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        node_sysid = cluster.get_pg_system_id(node)
        assert node_sysid == real_sysid, (
            f"{node} system ID {node_sysid!r} does not match cluster {real_sysid!r}"
        )


# ---------------------------------------------------------------------------
# EC4b – node with wrong-epoch PG data wipes itself and rejoins
# ---------------------------------------------------------------------------


def test_wrong_epoch_pg_data_triggers_wipe_and_rejoin(
    cluster: ClusterManager, mock_api: MockApiClient
):
    """
    EC4b: A fresh replacement node bootstraps a new PG cluster (new system ID
    and /initialize key).  The surviving base nodes carry PG data from the old
    cluster epoch and cannot join via Patroni.  The daemon detects that their
    local PG system ID differs from /initialize and wipes their data so Patroni
    can pg_basebackup from the new primary.

    Setup
    -----
    1. Kill all three base nodes so the cluster has no quorum.
    2. Mock API exposes only test-node4 so it bootstraps a fresh single-member
       etcd and PG cluster (new system ID Y, /initialize = Y).
    3. Add base node IPs back to mock API and restart the base nodes.
       They still have old PG data (system ID X ≠ Y).
    4. The daemon on each base node detects X ≠ Y while test-node4 (or another
       base node that already healed) confirms Y → wipes PG data → Patroni
       pg_basebackups → node rejoins.
    """
    cluster.wait_for_healthy(expected_members=3, timeout=120)

    original_sysid = cluster.etcd_get("postgres-node1", "/patroni/postgres-cluster/initialize")
    assert original_sysid, "initialize key must be set"

    # --- Kill all base nodes ---
    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        cluster.kill_node(node)
    time.sleep(5)

    # --- Spawn a completely fresh replacement node ---
    # test-node4 has no volumes: etcd and PG data are both empty.
    # With only itself in the mock API it will be the bootstrap candidate,
    # perform initdb (new system ID Y) and set /initialize = Y.
    mock_api.set_nodes(["172.20.0.13"])
    cluster.spawn_fresh_node("test-node4")

    # Wait until test-node4 is healthy as a single-node cluster.
    cluster.wait_for_healthy(expected_members=1, timeout=180)

    new_sysid = cluster.etcd_get("test-node4", "/patroni/postgres-cluster/initialize")
    assert new_sysid, "test-node4 must set /initialize after bootstrap"
    assert new_sysid != original_sysid, (
        "test-node4 should have bootstrapped a NEW system ID; "
        f"both old and new are {original_sysid!r}"
    )

    # --- Restart base nodes with their stale PG data ---
    mock_api.set_nodes(["172.20.0.10", "172.20.0.11", "172.20.0.12", "172.20.0.13"])
    for node in ["postgres-node1", "postgres-node2", "postgres-node3"]:
        cluster.start_node(node)

    # --- Expect auto-heal ---
    # Each base node's daemon detects its local PG system ID (original_sysid) ≠
    # /initialize (new_sysid), finds test-node4 (or a peer that already healed)
    # confirming new_sysid, wipes PG data, restarts Patroni → pg_basebackup.
    cluster.wait_for_healthy(expected_members=4, timeout=480)

    # All four nodes must carry the NEW system ID after healing.
    all_nodes = ["postgres-node1", "postgres-node2", "postgres-node3", "test-node4"]
    for node in all_nodes:
        node_sysid = cluster.get_pg_system_id(node)
        assert node_sysid == new_sysid, (
            f"{node} system ID {node_sysid!r} should be {new_sysid!r} after healing"
        )

    rows = cluster.exec_sql("SELECT 1")
    assert rows == [(1,)]
