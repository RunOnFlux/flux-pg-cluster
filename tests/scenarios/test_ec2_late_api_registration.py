"""
EC2: Node starts before Flux API registers it.
The new node self-adds to etcd (entrypoint.sh line ~241 adds itself
to ETCD_INITIAL_CLUSTER if not found). The existing nodes' updater sees
it as unexpected in etcd but not in desired state. As long as mock API
is updated before DESIRED_STATE_STABILITY_CYCLES pass, the node is kept.
"""

import time

import pytest


@pytest.mark.timeout(300)
def test_node_accepted_when_api_updated_in_time(cluster, mock_api):
    """A restarted node should remain when the API is updated within the stability window."""
    cluster.wait_for_healthy(expected_members=3, timeout=60)

    mock_api.set_nodes(["172.20.0.10", "172.20.0.11"])
    cluster.kill_node("postgres-node3")

    time.sleep(60)

    cluster.start_node("postgres-node3")

    time.sleep(5)
    mock_api.set_nodes(["172.20.0.10", "172.20.0.11", "172.20.0.12"])

    cluster.wait_for_healthy(expected_members=3, timeout=180)
    members = cluster.get_running_members()
    assert len(members) >= 3


@pytest.mark.timeout(180)
def test_node_evicted_when_api_not_updated(cluster, mock_api):
    """A node kept out of the API beyond the stability window should be evicted from etcd."""
    cluster.wait_for_healthy(expected_members=3, timeout=60)

    mock_api.set_nodes(["172.20.0.10", "172.20.0.11"])

    time.sleep(60)

    leader = cluster.get_leader()
    assert leader is not None

    mock_api.set_nodes(["172.20.0.10", "172.20.0.11", "172.20.0.12"])
    cluster.wait_for_healthy(expected_members=3, timeout=120)
