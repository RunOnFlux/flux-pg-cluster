"""Late discovery registration and persistent omission of healthy members.

Discovery omissions retain live members; unreachable omitted members can be
removed only with quorum and repeated direct failures.
"""

import time


def test_node_accepted_when_api_updated_in_time(cluster, mock_api):
    """A restarted node should remain when the API is updated within the stability window."""
    # Give generous time — node3 may still be joining from a previous test run
    cluster.wait_for_healthy(expected_members=3, timeout=300)

    mock_api.set_nodes(["172.20.0.10", "172.20.0.11"])
    cluster.kill_node("postgres-node3")

    # Wait through stability and repeated-failure windows while node3 is stopped
    time.sleep(60)

    cluster.start_node("postgres-node3")

    time.sleep(5)
    mock_api.set_nodes(["172.20.0.10", "172.20.0.11", "172.20.0.12"])

    # Node3 needs ~120s etcd peer discovery + ~60s patroni init after restart
    cluster.wait_for_healthy(expected_members=3, timeout=300)
    members = cluster.get_running_members()
    assert len(members) >= 3


def test_healthy_node_preserved_when_api_not_updated(cluster, mock_api):
    """A persistently incomplete discovery list must not evict a live voter."""
    cluster.wait_for_healthy(expected_members=3, timeout=300)
    before = cluster.etcd_cmd("postgres-node1", "member", "list")
    mock_api.set_nodes(["172.20.0.10", "172.20.0.11"])
    time.sleep(75)
    after = cluster.etcd_cmd("postgres-node1", "member", "list")
    assert before == after
    cluster.wait_for_healthy(expected_members=3, timeout=60)
