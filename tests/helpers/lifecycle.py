import json
import re
import subprocess

def start_new_cluster(project_dir, compose_cmd):
    """Only the harness authorizes initdb; all running services get the pin."""
    # Remove a previous module's pin before deliberately creating new volumes.
    # Compose commands use two -f pairs: docker compose -f X -f Y.
    base = ["docker", "compose", "-f", "docker-compose.yml", "-f", "docker-compose.test.yml"]
    result = subprocess.run(
        base + ["run", "--rm", "--no-deps", "postgres-node1", "sh", "-ec",
                'umask 077; openssl rand -hex 32 > /tmp/create-token; '
                '/app/flux-agent create-cluster --confirm-new-cluster "$APP_NAME" '
                '--operator-token-file /tmp/create-token'],
        cwd=project_dir, check=True, text=True, capture_output=True,
    )
    found = re.search(r"EXPECTED_POSTGRES_SYSTEM_ID=(\d+)", result.stdout)
    if not found:
        raise RuntimeError("explicit creation did not return a PostgreSQL identity: " + result.stdout)
    pin = project_dir / ".pytest_cache" / "cluster-pin.json"
    pin.parent.mkdir(exist_ok=True)
    pin.write_text(json.dumps({"services": {
        f"postgres-node{i}": {"environment": {"EXPECTED_POSTGRES_SYSTEM_ID": found.group(1)}}
        for i in range(1, 4)
    }}))
    compose_cmd[:] = base + ["-f", str(pin)]
    subprocess.run(compose_cmd + ["up", "-d", "--no-build"], cwd=project_dir, check=True)
