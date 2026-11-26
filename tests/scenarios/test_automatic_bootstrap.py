"""Real entrypoint bootstrap, without an operator-created seed or identity ENV."""

import json
import subprocess
import time

import pytest

from tests.helpers.cluster import BASE_NODES, ClusterManager
from tests.helpers.mock_api import MockApiClient


@pytest.fixture(scope="module")
def running_cluster(project_dir, compose_cmd, built_image, docker_client):
    previous = compose_cmd[:]
    override = project_dir / ".pytest_cache" / "automatic-bootstrap.json"
    override.parent.mkdir(exist_ok=True)
    override.write_text(json.dumps({"services": {
        name: {"environment": {
            "AUTO_BOOTSTRAP_IF_FRESH": "true",
            "ALLOW_NEW_CLUSTER_BOOTSTRAP": "false",
            "EXPECTED_POSTGRES_SYSTEM_ID": "",
            "BOOTSTRAP_PEER_CONFIRM_CYCLES": "2",
            "BOOTSTRAP_PEER_PROBE_INTERVAL_SECONDS": "1",
            "BOOTSTRAP_PEER_PROBE_TIMEOUT_SECONDS": "2",
            "POSTGRES_DB": "automatic_bootstrap_app",
        }} for name in BASE_NODES
    }}))
    compose_cmd[:] = previous + ["-f", str(override)]
    try:
        subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=True)
        # The Flux spec and location list declare three nodes, but the third is
        # initially absent. Two healthy empty peers must not initialize early.
        subprocess.run(compose_cmd + ["up", "-d", "--no-build", "mock-api", "postgres-node1", "postgres-node2"], cwd=project_dir, check=True)
        api = MockApiClient()
        for _ in range(50):
            if api.health():
                break
            time.sleep(.2)
        # Also exercise an incomplete location response: it lists two nodes
        # while app specifications continue to declare three instances.
        api.set_nodes([BASE_NODES[name].ip for name in list(BASE_NODES)[:2]])
        time.sleep(45)
        for name in list(BASE_NODES)[:2]:
            result = docker_client.containers.get(BASE_NODES[name].container_name).exec_run(
                ["sh", "-ec", "test ! -e /var/lib/postgresql/data/global/pg_control"])
            assert result.exit_code == 0, "incomplete placement initialized PostgreSQL"
        api.reset()
        subprocess.run(compose_cmd + ["up", "-d", "--no-build", "postgres-node3"], cwd=project_dir, check=True)
        yield
    finally:
        subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
        compose_cmd[:] = previous


def test_normal_entrypoint_bootstraps_once_and_preserves_data_on_restart(cluster, project_dir, compose_cmd):
    original = cluster.get_pg_system_id("postgres-node1")
    for name in BASE_NODES:
        assert cluster.get_pg_system_id(name) == original
    assert cluster.exec_sql("SELECT count(*) FROM pg_database WHERE datname = 'automatic_bootstrap_app'") == [(1,)]
    cluster.exec_sql("CREATE TABLE automatic_bootstrap_sentinel (value text)")
    cluster.exec_sql("INSERT INTO automatic_bootstrap_sentinel VALUES ('preserved')")
    subprocess.run(compose_cmd + ["restart"], cwd=project_dir, check=True)
    cluster.wait_for_healthy(timeout=300)
    assert cluster.exec_sql("SELECT value FROM automatic_bootstrap_sentinel") == [("preserved",)]
    assert all(cluster.get_pg_system_id(name) == original for name in BASE_NODES)


def test_all_placement_volumes_lost_can_bootstrap_a_new_database(cluster, project_dir, compose_cmd, docker_client):
    original = cluster.get_pg_system_id("postgres-node1")
    # This is an intentional loss of ALL disposable test volumes. It models the
    # opted-in availability policy; the agent never deletes surviving PGDATA.
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=True)
    subprocess.run(compose_cmd + ["up", "-d", "--no-build"], cwd=project_dir, check=True)
    recovered = ClusterManager(docker_client)
    recovered.wait_for_healthy(timeout=360)
    new_id = recovered.get_pg_system_id("postgres-node1")
    assert new_id != original
    assert all(recovered.get_pg_system_id(name) == new_id for name in BASE_NODES)
    assert recovered.exec_sql("SELECT to_regclass('public.automatic_bootstrap_sentinel')") == [(None,)]
    assert recovered.exec_sql("SELECT count(*) FROM pg_database WHERE datname = 'automatic_bootstrap_app'") == [(1,)]
