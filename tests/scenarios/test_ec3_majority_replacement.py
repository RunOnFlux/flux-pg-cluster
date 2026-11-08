"""Majority replacement preserves data after explicit operator fencing.

Loss of a discovery entry cannot prove that an old writer stopped. The tests
stop both old nodes and explicitly retire their IPs before rebuilding DCS.
"""

import subprocess
from tests.helpers.lifecycle import start_new_cluster

import docker as docker_sdk
import pytest

from tests.helpers.cluster import ClusterManager
from tests.helpers.mock_api import MockApiClient


@pytest.fixture(scope="function")
def running_cluster(project_dir, compose_cmd, built_image):
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
    start_new_cluster(project_dir, compose_cmd)
    yield
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)


@pytest.fixture(scope="function")
def cluster(running_cluster):
    manager = ClusterManager(docker_sdk.from_env())
    manager.wait_for_healthy(timeout=300)
    yield manager
    manager.cleanup_dynamic()


@pytest.fixture(scope="function")
def mock_api(cluster):
    # Depend on cluster (not just running_cluster) so the mock API container is
    # fully ready before reset_mock_api (autouse) runs its reset call.
    return MockApiClient()


def test_majority_replacement_recovery(cluster, mock_api):
    """After operator-confirmed fencing, replacements recover the preserved database."""
    cluster.wait_for_healthy(expected_members=3, timeout=120)
    original_id = cluster.get_pg_system_id("postgres-node1")
    original_leader = cluster.get_leader()
    assert original_leader is not None

    cluster.kill_node("postgres-node2")
    cluster.kill_node("postgres-node3")

    mock_api.set_nodes(["172.20.0.10", "172.20.0.13", "172.20.0.14"])

    cluster.spawn_fresh_node("test-node4")
    cluster.spawn_fresh_node("test-node5")
    cluster.retire_fenced_nodes(["172.20.0.11", "172.20.0.12"])

    cluster.wait_for_healthy(expected_members=3, timeout=300)

    assert cluster.get_pg_system_id("postgres-node1") == original_id
    new_leader = cluster.get_leader()
    assert new_leader is not None

    rows = cluster.exec_sql("SELECT 1")
    assert rows == [(1,)]

    members = cluster.get_running_members()
    assert len(members) >= 3


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
    cluster.retire_fenced_nodes(["172.20.0.11", "172.20.0.12"])

    cluster.wait_for_healthy(expected_members=3, timeout=300)

    rows = cluster.exec_sql("SELECT val FROM test_survive WHERE val = 'before_replacement'")
    assert len(rows) == 1
    assert rows[0][0] == "before_replacement"

    cluster.exec_sql("DROP TABLE IF EXISTS test_survive")
