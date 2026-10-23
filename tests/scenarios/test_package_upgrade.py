"""Rolling package updates preserve existing PostgreSQL and etcd volumes."""

import json
import subprocess

import pytest

from tests.helpers.cluster import BASE_NODES
from tests.helpers.lifecycle import start_new_cluster


@pytest.fixture(scope="module")
def upgrade_images(request, docker_client, built_image):
    source = request.config.getoption("--upgrade-from-image")
    if not source:
        pytest.skip("provide --upgrade-from-image with the pre-update safety-enabled image")
    old = docker_client.images.get(source)
    expected = request.config.getoption("--postgres-major")
    assert old.labels.get("runonflux.postgres.major") == expected
    new = docker_client.images.get("flux-pg-cluster-postgres-node1:latest")
    assert new.labels.get("runonflux.postgres.major") == expected
    assert old.id != new.id, "upgrade must replace the runtime image"
    return old.id, new.id


@pytest.fixture(scope="module")
def running_cluster(project_dir, compose_cmd, upgrade_images, built_image):
    previous = compose_cmd[:]
    pin = project_dir / ".pytest_cache" / "cluster-pin.json"
    base = previous[:-2] if previous[-2:] == ["-f", str(pin)] else previous
    override = project_dir / ".pytest_cache" / "upgrade-images.json"
    override.parent.mkdir(exist_ok=True)
    override.write_text(json.dumps({"services": {
        name: {"image": upgrade_images[0]} for name in BASE_NODES
    }}))
    compose_cmd[:] = base + ["-f", str(override)]
    try:
        subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=True)
        start_new_cluster(project_dir, compose_cmd)
        yield
    finally:
        subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
        compose_cmd[:] = previous


def test_rolling_upgrade_preserves_existing_volumes(cluster, project_dir, compose_cmd, upgrade_images):
    original_id = cluster.get_pg_system_id("postgres-node1")
    original_members = cluster.etcd_cmd("postgres-node1", "member", "list")
    cluster.exec_sql("CREATE TABLE package_upgrade_sentinel (value text)")
    cluster.exec_sql("INSERT INTO package_upgrade_sentinel VALUES ('before_upgrade')")
    for name in BASE_NODES:
        version = cluster._container(name).exec_run(["etcd", "--version"])
        assert version.exit_code == 0 and b"3.5.31" in version.output

    leader = cluster.get_leader()
    primary = next(name for name, node in BASE_NODES.items() if "node-" + node.ip.replace(".", "-") == leader)
    order = [name for name in BASE_NODES if name != primary] + [primary]
    override = project_dir / ".pytest_cache" / "upgrade-images.json"
    config = json.loads(override.read_text())
    for name in order:
        config["services"][name]["image"] = upgrade_images[1]
        override.write_text(json.dumps(config))
        subprocess.run(compose_cmd + ["up", "-d", "--no-deps", "--no-build", "--force-recreate", name], cwd=project_dir, check=True)
        cluster.wait_for_healthy(expected_members=3, timeout=300)
        assert cluster.get_pg_system_id(name) == original_id
        assert cluster.exec_sql("SELECT value FROM package_upgrade_sentinel") == [("before_upgrade",)]
        assert cluster.etcd_cmd("postgres-node1", "member", "list") == original_members
        container = cluster._container(name)
        assert container.image.id == upgrade_images[1]
        version = container.exec_run(["etcd", "--version"])
        assert version.exit_code == 0 and b"3.5.35" in version.output
        python = container.exec_run(["python3", "-c", "import sys; print(sys.prefix)"])
        assert python.exit_code == 0 and python.output.strip() == b"/opt/patroni"
