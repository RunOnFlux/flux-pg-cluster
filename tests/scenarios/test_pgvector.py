"""pgvector is available, physically replicated, and usable after failover."""

from __future__ import annotations

import time

import psycopg2

from tests.helpers.cluster import BASE_NODES


def _query_node(node_name: str, query: str) -> list:
    cfg = BASE_NODES[node_name]
    with psycopg2.connect(
        host=cfg.ip,
        port=cfg.pg_port,
        dbname="postgres",
        user="postgres",
        password="supersecretpassword",
        connect_timeout=10,
        sslmode="require",
    ) as connection:
        connection.autocommit = True
        with connection.cursor() as cursor:
            cursor.execute(query)
            return cursor.fetchall() if cursor.description else []


def _wait_for_vector_data_on_all_nodes(timeout: int = 120) -> None:
    deadline = time.time() + timeout
    last_errors: dict[str, str] = {}
    while time.time() < deadline:
        ready = True
        for node_name in BASE_NODES:
            try:
                rows = _query_node(
                    node_name,
                    """
                    SELECT e.extversion, v.label, to_regclass('public.pgvector_items_embedding_idx') IS NOT NULL
                    FROM pg_extension AS e
                    CROSS JOIN pgvector_items AS v
                    WHERE e.extname = 'vector'
                    ORDER BY v.embedding <-> '[1,2,3]'::vector
                    LIMIT 1
                    """,
                )
                if not rows or rows[0][1:] != ("nearest", True):
                    ready = False
                    last_errors[node_name] = f"unexpected rows: {rows!r}"
            except psycopg2.Error as exc:
                ready = False
                last_errors[node_name] = str(exc)
        if ready:
            return
        time.sleep(2)
    raise AssertionError(f"pgvector state did not replicate to every node: {last_errors}")


def test_pgvector_replication_and_failover(cluster, mock_api):
    """A vector index and its data remain queryable after primary promotion."""
    old_leader = cluster.get_leader()
    assert old_leader is not None

    leader_ip = old_leader.replace("node-", "").replace("-", ".")
    old_leader_node = next(
        name for name, cfg in BASE_NODES.items() if cfg.ip == leader_ip
    )

    cluster.exec_sql(
        """
        CREATE EXTENSION IF NOT EXISTS vector;
        CREATE TABLE pgvector_items (
            id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            label text NOT NULL,
            embedding vector(3) NOT NULL
        );
        INSERT INTO pgvector_items (label, embedding)
        VALUES ('nearest', '[1,2,3]'), ('farther', '[8,9,10]');
        CREATE INDEX pgvector_items_embedding_idx
            ON pgvector_items USING hnsw (embedding vector_l2_ops);
        """
    )
    _wait_for_vector_data_on_all_nodes()

    cluster.kill_node(old_leader_node)
    try:
        new_leader = cluster.wait_for_leader_change(old_leader, timeout=120)
        assert new_leader != old_leader
        assert cluster.exec_sql(
            """
            SELECT label
            FROM pgvector_items
            ORDER BY embedding <-> '[1.1,2.1,3.1]'::vector
            LIMIT 1
            """
        ) == [("nearest",)]
    finally:
        cluster.start_node(old_leader_node)

    cluster.wait_for_healthy(expected_members=3, timeout=240)
