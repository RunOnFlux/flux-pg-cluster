"""
EC3: 2 of 3 nodes go down simultaneously and fresh replacements spawn.

Critical scenario: surviving node A has only 1/3 etcd members → NO QUORUM.
New nodes B' and C' try etcdctl member add → FAILS (needs quorum).
A's updater detects no-quorum for DESIRED_STATE_STABILITY_CYCLES consecutive
cycles with stale members pending → writes /tmp/force-new-cluster flag,
restarts etcd with --force-new-cluster.
New nodes can then join the single-member cluster.

Timing with test overrides (UPDATE_INTERVAL=15s, STABILITY_CYCLES=2):
- No-quorum detected: ~15s after kill
- force-new-cluster trigger: ~30s after kill (2 cycles)
- etcd restart: ~5s
- New nodes join + Patroni elects leader: ~90s
- Total: ~150s, test timeout: 300s
"""

import subprocess

import docker as docker_sdk
import pytest

from tests.helpers.cluster import ClusterManager
from tests.helpers.mock_api import MockApiClient


@pytest.fixture(scope="function")
def running_cluster(project_dir, compose_cmd, built_image):
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
    subprocess.run(compose_cmd + ["up", "-d"], cwd=project_dir, check=True)
    yield
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)


@pytest.fixture(scope="function")
def cluster(running_cluster):
    manager = ClusterManager(docker_sdk.from_env())
    manager.wait_for_healthy(timeout=300)
    yield manager
    manager.cleanup_dynamic()


@pytest.fixture(scope="function")
def mock_api(running_cluster):
    return MockApiClient()


@pytest.mark.timeout(360)
def test_majority_replacement_recovery(cluster, mock_api):
    """Replacing two dead nodes should recover the cluster via force-new-cluster."""
    cluster.wait_for_healthy(expected_members=3, timeout=120)
    original_leader = cluster.get_leader()
    assert original_leader is not None

    cluster.kill_node("postgres-node2")
    cluster.kill_node("postgres-node3")

    mock_api.set_nodes(["172.20.0.10", "172.20.0.13", "172.20.0.14"])

    cluster.spawn_fresh_node("test-node4")
    cluster.spawn_fresh_node("test-node5")

    cluster.wait_for_healthy(expected_members=3, timeout=300)

    new_leader = cluster.get_leader()
    assert new_leader is not None

    rows = cluster.exec_sql("SELECT 1")
    assert rows == [(1,)]

    members = cluster.get_running_members()
    assert len(members) >= 3


@pytest.mark.timeout(240)
def test_data_survives_majority_replacement(cluster, mock_api):
    """Data written before majority replacement should remain after recovery completes."""
    cluster.wait_for_healthy(expected_members=3, timeout=120)

    cluster.exec_sql("CREATE TABLE IF NOT EXISTS test_survive (id SERIAL, val TEXT)")
    cluster.exec_sql("INSERT INTO test_survive (val) VALUES ('before_replacement')")

    cluster.kill_node("postgres-node2")
    cluster.kill_node("postgres-node3")
    mock_api.set_nodes(["172.20.0.10", "172.20.0.13", "172.20.0.14"])
    cluster.spawn_fresh_node("test-node4")
    cluster.spawn_fresh_node("test-node5")

    cluster.wait_for_healthy(expected_members=3, timeout=300)

    rows = cluster.exec_sql("SELECT val FROM test_survive WHERE val = 'before_replacement'")
    assert len(rows) == 1
    assert rows[0][0] == "before_replacement"

    cluster.exec_sql("DROP TABLE IF EXISTS test_survive")
