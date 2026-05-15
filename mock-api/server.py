import asyncio
import copy
import json
import os
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field


app = FastAPI()

_state: dict[str, Any] = {
    "nodes": [],
    "delay_ms": 0,
    "error": False,
    "error_status": 500,
}
_initial_nodes: list[dict[str, Any]] = []


class NodesPayload(BaseModel):
    nodes: list[dict[str, Any]] = Field(default_factory=list)


class DelayPayload(BaseModel):
    ms: int = 0


class ErrorPayload(BaseModel):
    enabled: bool = False
    status: int = 500


@app.on_event("startup")
async def load_initial_nodes() -> None:
    global _initial_nodes

    initial_nodes_file = os.environ.get("INITIAL_NODES_FILE")
    if not initial_nodes_file:
        _initial_nodes = []
        _state["nodes"] = []
        return

    data = json.loads(Path(initial_nodes_file).read_text())
    nodes = data.get("data", data)
    if not isinstance(nodes, list):
        raise RuntimeError("INITIAL_NODES_FILE must contain a node list or a JSON object with a data field")

    _initial_nodes = copy.deepcopy(nodes)
    _state["nodes"] = copy.deepcopy(_initial_nodes)


@app.get("/apps/location/{app_name}")
async def app_location(app_name: str) -> dict[str, Any]:
    await asyncio.sleep(_state["delay_ms"] / 1000)
    if _state["error"]:
        raise HTTPException(status_code=_state["error_status"], detail="mock API error")
    return {"status": "success", "data": copy.deepcopy(_state["nodes"])}


@app.post("/admin/set-nodes")
async def set_nodes(payload: NodesPayload) -> dict[str, Any]:
    _state["nodes"] = copy.deepcopy(payload.nodes)
    return {"status": "ok", "nodes": copy.deepcopy(_state["nodes"])}


@app.post("/admin/set-delay")
async def set_delay(payload: DelayPayload) -> dict[str, Any]:
    _state["delay_ms"] = payload.ms
    return {"status": "ok", "delay_ms": _state["delay_ms"]}


@app.post("/admin/set-error")
async def set_error(payload: ErrorPayload) -> dict[str, Any]:
    _state["error"] = payload.enabled
    _state["error_status"] = payload.status
    return {
        "status": "ok",
        "error": _state["error"],
        "error_status": _state["error_status"],
    }


@app.post("/admin/reset")
async def reset_state() -> dict[str, Any]:
    _state["nodes"] = copy.deepcopy(_initial_nodes)
    _state["delay_ms"] = 0
    _state["error"] = False
    _state["error_status"] = 500
    return {"status": "ok", "state": copy.deepcopy(_state)}


@app.get("/admin/state")
async def admin_state() -> dict[str, Any]:
    return copy.deepcopy(_state)


@app.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok"}
