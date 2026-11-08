"""The selected PostgreSQL variant must survive explicit cluster creation."""

import json
from types import SimpleNamespace

import pytest

from tests.helpers.lifecycle import start_new_cluster


@pytest.fixture(autouse=True)
def reset_mock_api():
    yield


@pytest.mark.parametrize("major", ["14", "15"])
@pytest.mark.parametrize("previous_cluster", [False, True])
def test_creation_uses_selected_variant_without_previous_identity(tmp_path, monkeypatch, major, previous_cluster):
    base = ["docker", "compose", "-f", "docker-compose.yml", "-f", "docker-compose.test.yml"]
    if major == "15":
        base += ["-f", "docker-compose.pg15-smoke.yml"]
    pin = tmp_path / ".pytest_cache" / "cluster-pin.json"
    command = base + (["-f", str(pin)] if previous_cluster else [])
    calls = []

    def run(args, **kwargs):
        calls.append(args)
        return SimpleNamespace(stdout="EXPECTED_POSTGRES_SYSTEM_ID=123456789\n")

    monkeypatch.setattr("tests.helpers.lifecycle.subprocess.run", run)
    start_new_cluster(tmp_path, command)

    assert calls[0][:len(base)] == base
    assert str(pin) not in calls[0]
    assert calls[1] == base + ["-f", str(pin), "up", "-d", "--no-build"]
    assert command == base + ["-f", str(pin)]
    environments = json.loads(pin.read_text())["services"]
    assert all(service["environment"]["EXPECTED_POSTGRES_SYSTEM_ID"] == "123456789" for service in environments.values())
