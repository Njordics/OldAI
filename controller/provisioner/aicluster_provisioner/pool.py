"""Pooled placement: one copy of a model split across several nodes.

llama.cpp's RPC mode lets a leader node offload blocks of layers to worker
nodes. This module decides which nodes form the pool and what share of the
layers each holds. Memory comes from GPUs first (fast), then system RAM.
Every member pays a fixed runtime overhead, so fewer members is better.

A node's GPU memory counts only when its llama.cpp can use the GPU
(`pool_gpu` in the node's fit data). The Slave image ships llama.cpp's CPU
build, so today pooled shares always come from RAM, even on nodes whose
GPU serves single-node models through Ollama.
"""

from __future__ import annotations

from .catalog import Variant
from .fit import Budget, MiB, fmt, need_bytes

MEMBER_OVERHEAD = 384 * MiB
DEFAULT_MAX_NODES = 4
POOL_RUNTIMES = {"ollama"}  # Ollama model layers are GGUF files llama.cpp loads


def _allocate(need: int, members: list[tuple[str, Budget]]) -> dict[str, tuple[int, int]] | None:
    """Spread `need` bytes over members, GPU memory first. Returns
    node_id -> (vram_used, ram_used), or None if it doesn't fit."""
    left = need
    out = {nid: [0, 0] for nid, _ in members}
    for nid, b in sorted(members, key=lambda m: -m[1].vram):
        take = min(left, max(b.vram - MEMBER_OVERHEAD, 0))
        out[nid][0] = take
        left -= take
    for nid, b in sorted(members, key=lambda m: -m[1].ram):
        if left <= 0:
            break
        room = b.ram - (MEMBER_OVERHEAD if out[nid][0] == 0 else 0)
        take = min(left, max(room, 0))
        out[nid][1] = take
        left -= take
    if left > 0:
        return None
    return {k: (v[0], v[1]) for k, v in out.items() if v[0] + v[1] > 0}


def _proportional(need: int, members: list[tuple[str, Budget]]) -> dict[str, tuple[int, int]] | None:
    """Spread `need` over every member in proportion to its room, from GPU
    memory when the GPUs together can hold it, else from GPU + RAM."""
    gpu = {nid: max(b.vram - MEMBER_OVERHEAD, 0) for nid, b in members}
    if sum(gpu.values()) >= need and all(gpu.values()):
        total = sum(gpu.values())
        return {nid: (need * cap // total, 0) for nid, cap in gpu.items()}
    room = {nid: max(b.vram - MEMBER_OVERHEAD, 0) + max(b.ram - (0 if b.vram > MEMBER_OVERHEAD else MEMBER_OVERHEAD), 0)
            for nid, b in members}
    total = sum(room.values())
    if total < need or not all(room.values()):
        return None
    out = {}
    for nid, b in members:
        part = need * room[nid] // total
        v = min(part, gpu[nid])
        out[nid] = (v, part - v)
    return out


def _choose(need: int, budgets: list[tuple[str, Budget]], max_nodes: int, gpu_only: bool):
    """Smallest prefix of nodes (biggest first) that holds `need`."""
    key = (lambda m: (-m[1].vram, -m[1].ram)) if gpu_only else (lambda m: (-(m[1].vram + m[1].ram), -m[1].vram))
    ordered = sorted(budgets, key=key)
    for n in range(2, min(max_nodes, len(ordered)) + 1):
        group = ordered[:n]
        cap = sum(max(b.vram - MEMBER_OVERHEAD, 0) for _, b in group)
        if not gpu_only:
            cap += sum(max(b.ram, 0) for _, b in group)
        if cap >= need:
            alloc = _allocate(need, group)
            if alloc and (not gpu_only or all(r == 0 for _, r in alloc.values())):
                return alloc
    return None


def _pool_budget(node: dict, exclude: set[str]) -> Budget:
    b = Budget.for_node(node, exclude)
    if not node.get("pool_gpu"):
        b = Budget(vram=0, ram=b.ram, disk=b.disk)
    return b


def pool_variant(v: Variant, nodes: list[dict], quant: str | None = None, max_nodes: int = DEFAULT_MAX_NODES,
                 exclude: set[str] | None = None, explicit: bool = False) -> dict:
    """Best pool for v. With explicit=True the pool uses exactly `nodes`
    (members that end up holding nothing are dropped)."""
    exclude = exclude or set()
    budgets = [(n["node_id"], _pool_budget(n, exclude)) for n in nodes]
    disk = {nid: b.disk for nid, b in budgets}
    candidates = []
    for rank, q in enumerate(v.quants):
        if q.runtime not in POOL_RUNTIMES or (quant and q.quant != quant):
            continue
        need = need_bytes(v, q)
        if explicit:
            # The admin chose these nodes: all of them take part.
            alloc = _proportional(need, budgets)
            gpu = bool(alloc) and all(r == 0 for _, r in alloc.values())
        else:
            alloc = _choose(need, budgets, max_nodes, gpu_only=True)
            gpu = alloc is not None
            if alloc is None:
                alloc = _choose(need, budgets, max_nodes, gpu_only=False)
        if not alloc:
            continue
        # Leader: biggest share among members with disk room for the file.
        by_share = sorted(alloc.items(), key=lambda kv: -(kv[1][0] + kv[1][1]))
        leader = next((nid for nid, _ in by_share if disk.get(nid) is None or disk[nid] >= q.size), None)
        if leader is None:
            continue
        members = []
        for nid, (vram, ram) in by_share:
            used = vram + ram
            members.append({
                # The runtime's overhead sits where the member's share does.
                "node_id": nid, "share": round(used / need, 4),
                "vram": vram + (MEMBER_OVERHEAD if vram else 0), "ram": ram + (0 if vram else MEMBER_OVERHEAD),
                "placement": "gpu" if ram == 0 else ("cpu" if vram == 0 else "hybrid"), "leader": nid == leader,
            })
        candidates.append((0 if gpu else 1, rank, len(members), q, need, members))
    if not candidates:
        total = sum(max(b.vram, 0) + max(b.ram, 0) for _, b in budgets)
        return {"model_id": v.id, "fits": False, "members": [],
                "reason": f"not enough memory even across {min(len(budgets), max_nodes) if not explicit else len(budgets)} nodes ({fmt(total)} available)"}
    candidates.sort(key=lambda c: c[:3])
    gpu_rank, _, _, q, need, members = candidates[0]
    names = len(members)
    return {
        "model_id": v.id, "fits": True, "quant": q.quant, "runtime": "llamacpp", "ref": q.ref, "size": q.size,
        "need": need, "members": members, "pooled": names > 1,
        "reason": (f"split across {names} nodes" + (" on GPUs" if gpu_rank == 0 else " (GPU + RAM)")) if names > 1
        else "fits on one node; no need to pool",
    }
