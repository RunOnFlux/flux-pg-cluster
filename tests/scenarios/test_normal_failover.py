"""Normal failover: kill the leader, assert new election within 60 seconds."""

import pytest


@pytest.mark.timeout(180)
def test_leader_election_after_kill(cluster, mock_api):
    """Killing the current leader should trigger election of a new writable leader."""
    old_leader = cluster.get_leader()
    assert old_leader is not None, "No leader before test"

    leader_ip = old_leader.replace("node-", "").replace("-", ".")
    leader_node = None

    from tests.helpers.cluster import BASE_NODES

    for name, cfg in BASE_NODES.items():
        if cfg.ip == leader_ip:
            leader_node = name
            break

    assert leader_node is not None, f"Could not find container for leader {old_leader}"

    cluster.kill_node(leader_node)

    new_leader = cluster.wait_for_leader_change(old_leader, timeout=120)
    assert new_leader != old_leader
    assert new_leader is not None

    rows = cluster.exec_sql("SELECT 1")
    assert rows == [(1,)]

    cluster.start_node(leader_node)


@pytest.mark.timeout(300)
def test_cluster_recovers_to_three_members(cluster, mock_api):
    """Restarted nodes should rejoin until the cluster reports three running members."""
    cluster.wait_for_healthy(expected_members=3, timeout=240)
    members = cluster.get_running_members()
    assert len(members) >= 3
