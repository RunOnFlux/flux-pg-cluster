from __future__ import annotations

import subprocess
from pathlib import Path

import docker
import pytest
import urllib3

from tests.helpers.cluster import ClusterManager
from tests.helpers.mock_api import MockApiClient

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)


@pytest.fixture(scope="session")
def docker_client():
    return docker.from_env()


@pytest.fixture(scope="session")
def project_dir() -> Path:
    return Path("/root/flux-pg-cluster")


@pytest.fixture(scope="session")
def compose_cmd() -> list[str]:
    return ["docker", "compose", "-f", "docker-compose.yml", "-f", "docker-compose.test.yml"]


@pytest.fixture(scope="session")
def pg_password(project_dir: Path) -> str:
    env_file = project_dir / ".env"
    for line in env_file.read_text().splitlines():
        if line.startswith("POSTGRES_SUPERUSER_PASSWORD="):
            return line.split("=", 1)[1].strip()
    raise RuntimeError("POSTGRES_SUPERUSER_PASSWORD not found in .env")


@pytest.fixture(scope="session")
def built_image(project_dir: Path, compose_cmd: list[str]):
    subprocess.run(compose_cmd + ["build"], cwd=project_dir, check=True)
    return True


@pytest.fixture(scope="module")
def running_cluster(project_dir: Path, compose_cmd: list[str], built_image):
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)
    subprocess.run(compose_cmd + ["up", "-d"], cwd=project_dir, check=True)
    yield
    subprocess.run(compose_cmd + ["down", "-v", "--remove-orphans"], cwd=project_dir, check=False)


@pytest.fixture(scope="module")
def cluster(running_cluster, docker_client):
    manager = ClusterManager(docker_client)
    manager.wait_for_healthy(timeout=300)
    yield manager
    manager.cleanup_dynamic()


@pytest.fixture(scope="module")
def mock_api(running_cluster):
    return MockApiClient("http://172.20.0.5:80")


@pytest.fixture(scope="function", autouse=True)
def reset_mock_api(mock_api: MockApiClient):
    mock_api.reset()
    yield
