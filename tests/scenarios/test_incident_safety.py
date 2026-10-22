"""Regression coverage for incomplete discovery and loss of local etcd state."""

import time

from tests.helpers.cluster import BASE_NODES


def test_explicit_creation_keeps_application_roles_and_database(cluster):
    assert cluster.exec_sql("SELECT rolcanlogin, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname='admin'") == [(True, True, True)]
    env = dict(item.split("=",1) for item in cluster._container("postgres-node1").attrs["Config"]["Env"] if "=" in item)
    database = env.get("POSTGRES_DB", "postgres")
    assert cluster.exec_sql("SELECT current_database()", dbname=database) == [(database,)]


def test_two_empty_replacements_cannot_initialize_another_cluster(cluster, mock_api):
    cluster.exec_sql("CREATE TABLE incident_sentinel (value text)")
    cluster.exec_sql("INSERT INTO incident_sentinel VALUES ('original')")
    original_id = cluster.get_pg_system_id("postgres-node1")
    original_members = cluster.etcd_cmd("postgres-node1", "member", "list")
    try:
        # All original nodes remain healthy, but discovery reports only empty
        # replacements. Their inherited deployment pin must not become initdb.
        mock_api.set_nodes(["172.20.0.13", "172.20.0.14"])
        cluster.spawn_fresh_node("test-node4")
        cluster.spawn_fresh_node("test-node5")
        time.sleep(90)  # Beyond discovery, confirmation, and membership windows.
        for name in ("test-node4", "test-node5"):
            result = cluster._container(name).exec_run(
                ["sh", "-c", "test ! -e /var/lib/postgresql/data/global/pg_control"]
            )
            assert result.exit_code == 0, result.output
        assert cluster.etcd_cmd("postgres-node1", "member", "list") == original_members
        assert cluster.get_pg_system_id("postgres-node1") == original_id
        assert cluster.exec_sql("SELECT value FROM incident_sentinel") == [("original",)]
    finally:
        cluster.cleanup_dynamic()
        mock_api.reset()
    cluster.wait_for_healthy(timeout=120)


def test_registered_member_with_lost_etcd_state_is_replaced_as_learner(cluster):
    leader = cluster.get_leader()
    victim = next(name for name, cfg in BASE_NODES.items() if "node-" + cfg.ip.replace(".", "-") != leader)
    original_id = cluster.get_pg_system_id(victim)
    original_members = cluster.etcd_cmd("postgres-node1", "member", "list")
    container = cluster._container(victim)
    result = container.exec_run(["supervisorctl", "stop", "etcd"])
    assert result.exit_code == 0, result.output
    result = container.exec_run(["mkdir", "-p", "/var/lib/etcd/.flux-etcd-archives"])
    assert result.exit_code == 0, result.output
    result = container.exec_run(["mv", "/var/lib/etcd/member", "/var/lib/etcd/.flux-etcd-archives/lost-member-test"])
    assert result.exit_code == 0, result.output
    result = container.exec_run(["supervisorctl", "start", "etcd"])
    assert result.exit_code == 0, result.output
    deadline = time.time() + 180
    while time.time() < deadline:
        members = cluster.etcd_cmd("postgres-node1", "member", "list")
        if members != original_members and len(members.splitlines()) == 3 and "true" not in members:
            break
        time.sleep(3)
    else:
        raise AssertionError("lost-state member did not receive a new synchronized voter registration")
    cluster.wait_for_healthy(timeout=180)
    assert cluster.get_pg_system_id(victim) == original_id


def test_all_originals_unavailable_does_not_create_empty_database(cluster, mock_api):
    cluster.exec_sql("CREATE TABLE total_loss_sentinel (value text)")
    cluster.exec_sql("INSERT INTO total_loss_sentinel VALUES ('survives')")
    original_id = cluster.get_pg_system_id("postgres-node1")
    try:
        for node in BASE_NODES:
            cluster.kill_node(node)
        mock_api.set_nodes(["172.20.0.13", "172.20.0.14"])
        cluster.spawn_fresh_node("test-node4")
        cluster.spawn_fresh_node("test-node5")
        time.sleep(75)
        for name in ("test-node4", "test-node5"):
            result = cluster._container(name).exec_run(
                ["sh", "-c", "test ! -e /var/lib/postgresql/data/global/pg_control"]
            )
            assert result.exit_code == 0, result.output
    finally:
        cluster.cleanup_dynamic()
        mock_api.reset()
        for node in BASE_NODES:
            cluster.start_node(node)
    cluster.wait_for_healthy(timeout=240)
    assert cluster.get_pg_system_id("postgres-node1") == original_id
    assert cluster.exec_sql("SELECT value FROM total_loss_sentinel") == [("survives",)]


def test_total_dcs_loss_recovers_preserved_database_and_archived_state(cluster):
    original_id = cluster.get_pg_system_id("postgres-node1")
    original_members = cluster.etcd_cmd("postgres-node1", "member", "list")
    cluster.exec_sql("CREATE TABLE dcs_loss_sentinel (value text)")
    cluster.exec_sql("INSERT INTO dcs_loss_sentinel VALUES ('preserved')")
    # Prior incident tests observed these disposable replacements. They have
    # been stopped and removed; explicitly retire them before recovery.
    cluster.retire_fenced_nodes(["172.20.0.13", "172.20.0.14"])
    for name in BASE_NODES:
        result = cluster._container(name).exec_run(["supervisorctl", "stop", "etcd"])
        assert result.exit_code == 0, result.output
    for name in BASE_NODES:
        container = cluster._container(name)
        result = container.exec_run(["mkdir", "-p", "/var/lib/etcd/.flux-etcd-archives"])
        assert result.exit_code == 0, result.output
        result = container.exec_run(["mv", "/var/lib/etcd/member", "/var/lib/etcd/.flux-etcd-archives/total-loss-test"])
        assert result.exit_code == 0, result.output
    for name in BASE_NODES:
        result = cluster._container(name).exec_run(["supervisorctl", "start", "etcd"])
        assert result.exit_code == 0, result.output
    # Failsafe mode can keep SQL and cached Patroni membership healthy while
    # every DCS process is down. Require three new synchronized registrations
    # from the live etcd API, not just a writable cached PostgreSQL view.
    deadline = time.time() + 360
    while time.time() < deadline:
        try:
            members = cluster.etcd_cmd("postgres-node1", "member", "list", retries=1)
            lines = members.splitlines()
            if members != original_members and len(lines) == 3 and all(
                ", started, " in line and line.endswith(", false") for line in lines
            ):
                break
        except RuntimeError:
            pass
        time.sleep(3)
    else:
        raise AssertionError("total DCS loss did not produce three new synchronized voters")
    cluster.wait_for_healthy(timeout=360)
    assert cluster.get_pg_system_id("postgres-node1") == original_id
    assert cluster.exec_sql("SELECT value FROM dcs_loss_sentinel") == [("preserved",)]
