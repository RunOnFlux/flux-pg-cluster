"""Empty replacements learn an initialized database without a deployment ENV pin."""

import json
import subprocess
import time

import pytest
import psycopg2

from tests.helpers.cluster import BASE_NODES
from tests.helpers.lifecycle import start_new_cluster


@pytest.fixture(scope="module")
def running_cluster(project_dir, compose_cmd, built_image, request):
    previous = compose_cmd[:]
    legacy = request.config.getoption("--upgrade-from-image")
    if legacy:
        # Exercise the actual old deployment contract: auto-initialized 1.4,
        # persisted PGDATA/etcd, and no new identity environment variable.
        override = project_dir / ".pytest_cache" / "replacement-legacy.json"
        override.parent.mkdir(exist_ok=True)
        override.write_text(json.dumps({"services": {
            name: {"image": legacy, "environment": {
                "EXPECTED_POSTGRES_SYSTEM_ID": "",
                "AUTO_BOOTSTRAP_IF_FRESH": "true",
                "ALLOW_NEW_CLUSTER_BOOTSTRAP": "false",
            }} for name in BASE_NODES
        }}))
        compose_cmd[:] = previous + ["-f", str(override)]
    try:
        subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=True)
        if legacy:
            subprocess.run(compose_cmd + ["up", "-d", "--no-build"], cwd=project_dir, check=True)
        else:
            start_new_cluster(project_dir, compose_cmd)
        yield
    finally:
        subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
        compose_cmd[:] = previous


def replacement_rows(password):
    with psycopg2.connect(host="172.20.0.13", port=5432, dbname="postgres", user="postgres", password=password, sslmode="require", connect_timeout=5) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT value FROM unpinned_replacement_sentinel")
            return cur.fetchall()


def test_unpinned_replacement_clones_existing_data_and_remembers_identity(cluster, mock_api, pg_password):
    original_id = cluster.get_pg_system_id("postgres-node1")
    cluster.exec_sql("CREATE TABLE unpinned_replacement_sentinel (value text)")
    cluster.exec_sql("INSERT INTO unpinned_replacement_sentinel VALUES ('preserved')")
    leader = cluster.get_leader()
    victim = next(name for name, node in BASE_NODES.items() if "node-" + node.ip.replace(".", "-") != leader)
    cluster.kill_node(victim)
    # Leave the failed node advertised to exercise a stale discovery entry.
    mock_api.set_nodes([node.ip for node in BASE_NODES.values()] + ["172.20.0.13"])
    cluster.spawn_fresh_node("test-node4", unset_env=("EXPECTED_POSTGRES_SYSTEM_ID",), image_ref="flux-pg-cluster-postgres-node1:latest")
    cluster.wait_for_healthy(expected_members=3, timeout=300)
    container = cluster._container("test-node4")
    assert not any(item.startswith("EXPECTED_POSTGRES_SYSTEM_ID=") for item in container.attrs["Config"]["Env"])
    assert cluster.get_pg_system_id("test-node4") == original_id
    assert replacement_rows(pg_password) == [("preserved",)]
    result = container.exec_run(["cat", "/var/lib/etcd/.flux-pg-identity.json"])
    assert result.exit_code == 0, result.output
    assert json.loads(result.output)["system_id"] == original_id
    cluster.kill_node("test-node4")
    cluster.start_node("test-node4")
    cluster.wait_for_healthy(expected_members=3, timeout=240)
    assert cluster.get_pg_system_id("test-node4") == original_id
    assert replacement_rows(pg_password) == [("preserved",)]
    cluster.start_node(victim)
    cluster.wait_for_healthy(expected_members=4, timeout=180)


def test_unpinned_empty_nodes_do_not_initialize_after_total_outage(cluster, mock_api):
    original_id = cluster.get_pg_system_id("postgres-node1")
    cluster.cleanup_dynamic()
    mock_api.reset()
    cluster.wait_for_healthy(timeout=180)
    try:
        for name in BASE_NODES:
            cluster.kill_node(name)
        mock_api.set_nodes(["172.20.0.13", "172.20.0.14"])
        for name in ("test-node4", "test-node5"):
            cluster.spawn_fresh_node(name, unset_env=("EXPECTED_POSTGRES_SYSTEM_ID",), image_ref="flux-pg-cluster-postgres-node1:latest")
        time.sleep(75)
        for name in ("test-node4", "test-node5"):
            result = cluster._container(name).exec_run(["sh", "-ec", "test ! -e /var/lib/postgresql/data/global/pg_control; test ! -e /var/lib/etcd/.flux-pg-identity.json"])
            assert result.exit_code == 0, result.output
    finally:
        cluster.cleanup_dynamic()
        mock_api.reset()
        for name in BASE_NODES:
            cluster.start_node(name)
    cluster.wait_for_healthy(timeout=300)
    assert cluster.get_pg_system_id("postgres-node1") == original_id


def test_legacy_volume_rollout_needs_no_identity_env(cluster, project_dir, compose_cmd, request):
    if not request.config.getoption("--upgrade-from-image"):
        pytest.skip("provide the actual legacy image to exercise the legacy rollout")
    original_id = cluster.get_pg_system_id("postgres-node1")
    cluster.exec_sql("CREATE TABLE legacy_rollout_sentinel (value text)")
    cluster.exec_sql("INSERT INTO legacy_rollout_sentinel VALUES ('existing-data')")
    deadline = time.time() + 90
    while time.time() < deadline:
        original_members = cluster.etcd_cmd("postgres-node1", "member", "list")
        if len(original_members.splitlines()) == 3:
            break
        time.sleep(3)
    else:
        raise AssertionError("legacy cluster did not remove the stopped test replacement")
    leader = cluster.get_leader()
    primary = next(name for name, node in BASE_NODES.items() if "node-" + node.ip.replace(".", "-") == leader)
    order = [name for name in BASE_NODES if name != primary] + [primary]
    override = project_dir / ".pytest_cache" / "replacement-legacy.json"
    deployment = json.loads(override.read_text())
    for name in order:
        deployment["services"][name]["image"] = "flux-pg-cluster-postgres-node1:latest"
        assert deployment["services"][name]["environment"]["EXPECTED_POSTGRES_SYSTEM_ID"] == ""
        override.write_text(json.dumps(deployment))
        subprocess.run(compose_cmd + ["up", "-d", "--no-build", "--no-deps", "--force-recreate", name], cwd=project_dir, check=True)
        cluster.wait_for_healthy(timeout=300)
        assert cluster.get_pg_system_id(name) == original_id
        assert cluster.exec_sql("SELECT value FROM legacy_rollout_sentinel") == [("existing-data",)]
        assert cluster.etcd_cmd("postgres-node1", "member", "list") == original_members
