"""Recovery invariants that must be restored without operator intervention."""

from __future__ import annotations

import json
import time

from tests.helpers.cluster import BASE_NODES, ClusterManager


REQUIRED_HBA = {
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
    current["restore_test_sentinel"] = {"preserve": True}
    postgresql = current.setdefault("postgresql", {})
    postgresql["pg_hba"] = ["local all all peer"]
    postgresql["use_pg_rewind"] = False
    cluster.etcd_set(leader_service, config_key, json.dumps(current))

    # Simulate database-global state from an older backup, then remove the
    # per-container reconciliation marker so this primary is repaired again.
    cluster.exec_sql(
        "SELECT pg_terminate_backend(pid) FROM pg_stat_replication"
    )
    cluster.exec_sql("DROP ROLE IF EXISTS replicator")
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
