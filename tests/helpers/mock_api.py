from typing import List

import requests


class MockApiClient:
    def __init__(self, base_url: str = "http://172.20.0.5:80") -> None:
        self.base_url = base_url.rstrip("/")

    def set_nodes(self, ips: List[str]) -> None:
        nodes = [
            {
                "ip": ip,
                "name": f"node-{ip.replace('.', '-')}",
                "ports": [],
            }
            for ip in ips
        ]
        self.set_nodes_full(nodes)

    def set_nodes_full(self, nodes: list) -> None:
        response = requests.post(
            f"{self.base_url}/admin/set-nodes",
            json={"nodes": nodes},
            timeout=10,
        )
        response.raise_for_status()

    def get_nodes(self) -> list:
        response = requests.get(f"{self.base_url}/admin/state", timeout=10)
        response.raise_for_status()
        return response.json().get("nodes", [])

    def set_delay(self, ms: int) -> None:
        response = requests.post(
            f"{self.base_url}/admin/set-delay",
            json={"ms": ms},
            timeout=10,
        )
        response.raise_for_status()

    def set_error(self, enabled: bool, status: int = 500) -> None:
        response = requests.post(
            f"{self.base_url}/admin/set-error",
            json={"enabled": enabled, "status": status},
            timeout=10,
        )
        response.raise_for_status()

    def reset(self) -> None:
        response = requests.post(f"{self.base_url}/admin/reset", timeout=10)
        response.raise_for_status()

    def health(self) -> bool:
        try:
            response = requests.get(f"{self.base_url}/health", timeout=5)
            response.raise_for_status()
        except requests.RequestException:
            return False
        return response.json().get("status") == "ok"
