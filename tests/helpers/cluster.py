from __future__ import annotations

import time
from dataclasses import dataclass
from typing import Optional

import docker
import psycopg2
import requests
from docker.errors import NotFound


@dataclass(frozen=True)
class NodeConfig:
    container_name: str
    ip: str
    patroni_port: int = 8008
    pg_port: int = 5432
    etcd_client_port: int = 2379


BASE_NODES = {
    "postgres-node1": NodeConfig("flux-pg-cluster-postgres-node1-1", "172.20.0.10"),
    "postgres-node2": NodeConfig("flux-pg-cluster-postgres-node2-1", "172.20.0.11"),
    "postgres-node3": NodeConfig("flux-pg-cluster-postgres-node3-1", "172.20.0.12"),
}

EXTRA_NODES = {
    "test-node4": NodeConfig("test-node4", "172.20.0.13"),
    "test-node5": NodeConfig("test-node5", "172.20.0.14"),
}

NETWORK_NAME = "flux-pg-cluster_cluster_network"


HEALTHY_STATES = {"running", "streaming", "in archive recovery"}


class ClusterManager:
    def __init__(self, docker_client: docker.DockerClient) -> None:
        self.docker_client = docker_client
        self.dynamic_nodes: set[str] = set()

    def _config_for(self, node_name: str) -> NodeConfig:
        if node_name in BASE_NODES:
            return BASE_NODES[node_name]
        if node_name in EXTRA_NODES:
            return EXTRA_NODES[node_name]
        raise KeyError(f"Unknown node: {node_name}")

    def _all_node_names(self) -> list[str]:
        return list(BASE_NODES) + list(self.dynamic_nodes)

    def _container(self, node_name: str):
        return self.docker_client.containers.get(self._config_for(node_name).container_name)

    def patroni_status(self, node_name: str) -> Optional[dict]:
        cfg = self._config_for(node_name)
        try:
            response = requests.get(
                f"https://{cfg.ip}:{cfg.patroni_port}/patroni",
                timeout=5,
                verify=False,
            )
            response.raise_for_status()
        except requests.RequestException:
            return None
        return response.json()

    def cluster_status(self) -> Optional[dict]:
        for node_name in self._all_node_names():
            cfg = self._config_for(node_name)
            try:
                response = requests.get(
                    f"https://{cfg.ip}:{cfg.patroni_port}/cluster",
                    timeout=5,
                    verify=False,
                )
                response.raise_for_status()
                return response.json()
            except requests.RequestException:
                continue
        return None

    def get_leader(self) -> Optional[str]:
        status = self.cluster_status()
        members = status.get("members", []) if status else []
        for member in members:
            if member.get("role") in {"leader", "master", "primary"}:
                return member.get("name")

        for node_name in self._all_node_names():
            member_status = self.patroni_status(node_name)
            if member_status and member_status.get("role") in {"master", "primary"}:
                return member_status.get("name")
        return None

    def get_running_members(self) -> list[str]:
        status = self.cluster_status()
        members = status.get("members", []) if status else []
        running = [member.get("name") for member in members if member.get("state") in HEALTHY_STATES]
        if running:
            return [name for name in running if name]

        fallback = []
        for node_name in self._all_node_names():
            member_status = self.patroni_status(node_name)
            if member_status and member_status.get("state") in HEALTHY_STATES:
                fallback.append(member_status.get("name"))
        return [name for name in fallback if name]

    def wait_for_healthy(self, expected_members: int = 3, timeout: int = 300) -> bool:
        deadline = time.time() + timeout
        last_status = None
        while time.time() < deadline:
            # Use a single cluster_status call per iteration so leader and member
            # data come from the same consistent snapshot (avoids false-positives
            # from concurrent API calls to different nodes in a transitioning cluster).
            last_status = self.cluster_status()
            if last_status:
                members = last_status.get("members", [])
                leader_name = next(
                    (m.get("name") for m in members if m.get("role") in {"leader", "master", "primary"}),
                    None,
                )
                running = [m.get("name") for m in members if m.get("state") in HEALTHY_STATES]
                if leader_name and len(set(running)) >= expected_members:
                    # Verify the leader is actually reachable via SQL before
                    # declaring the cluster healthy. Patroni serves stale DCS
                    # data when etcd has no quorum (dead members may appear
                    # "running" in the cached view for 90+ seconds). Without
                    # this check, exec_sql would target the dead cached leader.
                    leader_ip = leader_name.replace("node-", "").replace("-", ".")
                    try:
                        with psycopg2.connect(
                            host=leader_ip,
                            port=5432,
                            dbname="postgres",
                            user="postgres",
                            password="supersecretpassword",
                            connect_timeout=5,
                            sslmode="require",
                        ) as conn:
                            conn.autocommit = True
                            with conn.cursor() as cur:
                                cur.execute("SELECT 1")
                        return True
                    except psycopg2.Error:
                        pass  # leader not yet reachable, keep waiting
            time.sleep(5)
        raise TimeoutError(
            f"Cluster did not become healthy within {timeout}s. Last status: {last_status}"
        )

    def wait_for_leader_change(self, old_leader: str, timeout: int = 120) -> str:
        deadline = time.time() + timeout
        while time.time() < deadline:
            new_leader = self.get_leader()
            if new_leader and new_leader != old_leader:
                return new_leader
            time.sleep(5)
        raise TimeoutError(f"Leader did not change from {old_leader} within {timeout}s")

    def kill_node(self, node_name: str) -> None:
        self._container(node_name).stop(timeout=0)

    def start_node(self, node_name: str) -> None:
        self._container(node_name).start()

    def pause_node(self, node_name: str) -> None:
        self._container(node_name).pause()

    def unpause_node(self, node_name: str) -> None:
        self._container(node_name).unpause()

    def spawn_fresh_node(self, node_name: str, base_env_from: str = "postgres-node1") -> None:
        cfg = self._config_for(node_name)
        base_container = self._container(base_env_from)
        image = base_container.image
        env = {
            key: value
            for item in base_container.attrs["Config"].get("Env", [])
            if "=" in item
            for key, value in [item.split("=", 1)]
        }

        try:
            stale = self.docker_client.containers.get(cfg.container_name)
            stale.remove(force=True)
        except NotFound:
            pass

        container = self.docker_client.containers.create(
            image=image.id,
            name=cfg.container_name,
            hostname=cfg.container_name,
            environment=env,
        )
        # Disconnect from the default bridge network before starting so that
        # `hostname -i` inside the container returns only the cluster network IP.
        # Without this, Docker attaches the bridge first and hostname -i returns
        # the bridge IP (172.17.0.x) as MY_IP, causing wrong etcd peer URLs.
        try:
            bridge = self.docker_client.networks.get("bridge")
            bridge.disconnect(container)
        except Exception:
            pass  # Not connected to bridge or bridge doesn't exist

        network = self.docker_client.networks.get(NETWORK_NAME)
        network.connect(container, ipv4_address=cfg.ip)
        container.start()
        self.dynamic_nodes.add(node_name)

    def remove_fresh_node(self, node_name: str) -> None:
        cfg = self._config_for(node_name)
        try:
            container = self.docker_client.containers.get(cfg.container_name)
            container.remove(force=True)
        except NotFound:
            pass
        self.dynamic_nodes.discard(node_name)

    def cleanup_dynamic(self) -> None:
        for node_name in list(self.dynamic_nodes):
            self.remove_fresh_node(node_name)

    def exec_sql(self, query: str, dbname: str = "postgres") -> list:
        deadline = time.time() + 120
        last_error: Exception | None = None
        while time.time() < deadline:
            leader_name = self.get_leader()
            if not leader_name:
                time.sleep(2)
                continue

            leader_ip = leader_name.replace("node-", "").replace("-", ".")
            try:
                with psycopg2.connect(
                    host=leader_ip,
                    port=5432,
                    dbname=dbname,
                    user="postgres",
                    password="supersecretpassword",
                    connect_timeout=10,
                    sslmode="require",
                ) as connection:
                    connection.autocommit = True
                    with connection.cursor() as cursor:
                        cursor.execute(query)
                        if cursor.description:
                            return cursor.fetchall()
                        return []
            except psycopg2.Error as exc:
                last_error = exc
                time.sleep(2)

        raise RuntimeError(f"SQL execution failed: {last_error}")
