"""Recovery invariants that must be restored without operator intervention."""

from __future__ import annotations

import json
import time

from tests.helpers.cluster import BASE_NODES, ClusterManager


REQUIRED_HBA = {
    "local all postgres peer",
    "hostssl replication replicator 0.0.0.0/0 cert clientcert=verify-full",
    "hostssl all all 0.0.0.0/0 md5",
    "host replication replicator 0.0.0.0/0 md5",
    "host all all 0.0.0.0/0 md5",
}


def _service_for_member(member_name: str) -> str:
    ip = member_name.removeprefix("node-").replace("-", ".")
    for service, node in BASE_NODES.items():
        if node.ip == ip:
            return service
    raise AssertionError(f"no test service for Patroni member {member_name}")


def test_restored_dcs_hba_and_roles_are_reconciled(cluster: ClusterManager):
    cluster.wait_for_healthy(expected_members=3, timeout=120)
    leader = cluster.get_leader()
    assert leader
    leader_service = _service_for_member(leader)

    config_key = "/patroni/postgres-cluster/config"
    current = json.loads(cluster.etcd_get(leader_service, config_key))

    # Simulate database-global state from an older backup while the existing
    # success marker still prevents the daemon from racing this setup.
    cluster.exec_sql(
        "SELECT pg_terminate_backend(pid) FROM pg_stat_replication"
    )
    cluster.exec_sql("DROP ROLE IF EXISTS replicator")

    current["restore_test_sentinel"] = {"preserve": True}
    postgresql = current.setdefault("postgresql", {})
    # Remove local socket access as well as the recovery-required host rules.
    # The daemon must restore its own peer-authentication path before it can
    # reconnect and reconcile the database roles below.
    postgresql["pg_hba"] = ["host all all 127.0.0.1/32 md5"]
    postgresql["use_pg_rewind"] = False
    cluster.etcd_set(leader_service, config_key, json.dumps(current))

    # Remove the per-container marker only after both faults are installed.
    # Patroni may briefly reject the daemon's first socket attempt while it
    # applies the repaired HBA; the next updater cycle must then succeed.
    container = cluster._container(leader_service)
    code, output = container.exec_run(
        ["rm", "-f", "/tmp/postgres-credentials-reconciled"]
    )
    assert code == 0, output

    deadline = time.time() + 120
    last_config = {}
    while time.time() < deadline:
        last_config = json.loads(cluster.etcd_get(leader_service, config_key))
        rules = set(last_config.get("postgresql", {}).get("pg_hba", []))
        role = cluster.exec_sql(
            "SELECT rolcanlogin, rolreplication "
            "FROM pg_roles WHERE rolname = 'replicator'"
        )
        if (
            REQUIRED_HBA.issubset(rules)
            and last_config["postgresql"].get("use_pg_rewind") is True
            and last_config.get("restore_test_sentinel") == {"preserve": True}
            and role == [(True, True)]
        ):
            break
        time.sleep(5)
    else:
        raise AssertionError(
            f"restore invariants were not reconciled; config={last_config}"
        )

    cluster.wait_for_healthy(expected_members=3, timeout=120)
