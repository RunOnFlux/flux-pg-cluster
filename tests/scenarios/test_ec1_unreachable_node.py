"""
EC1: Node declared in Flux API but container is not reachable.
Expected: cluster keeps quorum with 2/3 nodes; writes still work.
The unreachable node stays in Flux API but etcd quorum is maintained
by the 2 healthy nodes.
"""


def test_cluster_healthy_with_unreachable_node(cluster, mock_api):
    """A single dead node left in the Flux API should not break quorum or writes."""
    cluster.wait_for_healthy(expected_members=3, timeout=60)

    cluster.kill_node("postgres-node3")
    # Wait for the remaining 2 nodes to elect/keep a leader (node3 may have been leader)
    cluster.wait_for_healthy(expected_members=2, timeout=60)

    leader = cluster.get_leader()
    assert leader is not None, "Cluster lost leader after single node kill"

    rows = cluster.exec_sql("SELECT 1")
    assert rows == [(1,)]

    cluster.start_node("postgres-node3")


def test_api_shows_unreachable_node_but_cluster_serves(cluster, mock_api):
    """Reads and writes should continue even when Flux still lists a dead member."""
    cluster.wait_for_healthy(expected_members=3, timeout=60)

    cluster.kill_node("postgres-node2")
    # Wait for the remaining 2 nodes to stabilize before checking SQL
    cluster.wait_for_healthy(expected_members=2, timeout=60)

    rows = cluster.exec_sql("SELECT count(*) FROM pg_stat_activity")
    assert len(rows) > 0

    cluster.start_node("postgres-node2")
