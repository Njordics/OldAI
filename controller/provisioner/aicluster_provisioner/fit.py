"""Decides which quantisation of a model fits on a node, and where it runs.

Memory model (docs/architecture.md section 7):

    need = weights + kv_cache(ctx) + runtime overhead

Placement preference: fully on GPU (any quant that fits) > split GPU+RAM
(at least half on GPU) > CPU only. Within a placement the best-quality
quant wins. Budgets subtract what the node's resident models already use.
"""

from __future__ import annotations

from dataclasses import dataclass

from .catalog import Catalog, Quant, Variant

GiB = 1 << 30
MiB = 1 << 20

VRAM_USABLE = 0.92        # leave room for the driver/display
RAM_USABLE = 0.85
RAM_OS_RESERVE = 2 * GiB
DISK_RESERVE = 5 * GiB
MIN_GPU_SHARE = 0.5       # split placement only if at least half fits on GPU

PLACEMENT_RANK = {"gpu": 0, "hybrid": 1, "cpu": 2}


def kv_cache_bytes(v: Variant, size: int, ctx: int) -> int:
    a = v.arch or {}
    if all(k in a for k in ("layers", "kv_heads", "head_dim")):
        # K and V, f16.
        return 2 * a["layers"] * a["kv_heads"] * a["head_dim"] * ctx * 2
    # No architecture data: a conservative share of the weights.
    return int(size * 0.05) + 64 * MiB


def need_bytes(v: Variant, q: Quant, ctx: int | None = None) -> int:
    ctx = ctx or v.ctx_default
    overhead = 256 * MiB + int(q.size * 0.05)
    return q.size + kv_cache_bytes(v, q.size, ctx) + overhead


@dataclass
class Budget:
    vram: int
    ram: int
    disk: int | None  # None = unknown, don't check

    @staticmethod
    def for_node(node: dict, exclude: set[str]) -> "Budget":
        """exclude lists assignment ids to ignore (the model being swapped out)."""
        vram_total = sum(int(g.get("vram", 0)) for g in (node.get("gpus") or []) if g.get("usable", True))
        vram = int(vram_total * VRAM_USABLE)
        ram = int(int(node.get("ram_total") or 0) * RAM_USABLE) - RAM_OS_RESERVE
        disk_free = node.get("disk_free")
        disk = None if not disk_free else int(disk_free) - DISK_RESERVE
        for c in (node.get("committed") or []):
            if c.get("id") in exclude:
                continue
            if c.get("residency", "resident") == "resident":
                vram -= int(c.get("vram", 0))
                ram -= int(c.get("ram", 0))
            if disk is not None and not c.get("downloaded", False):
                disk -= int(c.get("size", 0))
        return Budget(vram=max(vram, 0), ram=max(ram, 0), disk=disk)


def place(need: int, size: int, b: Budget) -> tuple[str | None, int, int, str]:
    """Returns (placement, vram_use, ram_use, reason)."""
    if b.disk is not None and size > b.disk:
        return None, 0, 0, f"not enough disk ({fmt(size)} needed, {fmt(max(b.disk, 0))} free)"
    if b.vram and need <= b.vram:
        return "gpu", need, 0, "fits in GPU memory"
    if b.vram and b.vram >= need * MIN_GPU_SHARE and need - b.vram <= b.ram:
        return "hybrid", b.vram, need - b.vram, f"{int(b.vram / need * 100)}% on GPU, rest in RAM"
    if need <= b.ram:
        return "cpu", 0, need, "runs on CPU" if not b.vram else "too big for GPU; runs on CPU"
    return None, 0, 0, f"needs {fmt(need)} memory, {fmt(b.ram + b.vram)} available"


def fmt(n: int) -> str:
    return f"{n / GiB:.1f} GB" if n >= GiB else f"{n / MiB:.0f} MB"


def fit_variant(v: Variant, node: dict, quant: str | None = None, exclude: set[str] | None = None,
                runtimes: set[str] | None = None) -> dict:
    b = Budget.for_node(node, exclude or set())
    runtimes = runtimes or set(node.get("runtimes") or ["ollama"])
    options = []
    for q in v.quants:
        if q.runtime not in runtimes or (quant and q.quant != quant):
            continue
        need = need_bytes(v, q)
        placement, vram, ram, reason = place(need, q.size, b)
        options.append({
            "quant": q.quant, "runtime": q.runtime, "ref": q.ref, "size": q.size, "need": need,
            "fits": placement is not None, "placement": placement, "vram": vram, "ram": ram, "reason": reason,
        })
    fitting = [o for o in options if o["fits"]]
    fitting.sort(key=lambda o: (PLACEMENT_RANK[o["placement"]], [q.quant for q in v.quants].index(o["quant"])))
    best = fitting[0] if fitting else None
    result = {"model_id": v.id, "node_id": node.get("node_id"), "options": options}
    if best:
        result.update({k: best[k] for k in ("quant", "runtime", "ref", "size", "need", "placement", "vram", "ram", "reason")})
        result["fits"] = True
    else:
        result["fits"] = False
        result["reason"] = options[-1]["reason"] if options else "no source for this node's runtimes"
    return result


def fit(catalog: Catalog, req: dict) -> dict:
    variants = catalog.variants()
    wanted = req.get("models") or list(variants)
    unknown = [m for m in wanted if m not in variants]
    if unknown:
        raise KeyError(f"unknown models: {', '.join(unknown)}")
    quant = req.get("quant")
    exclude = set(req.get("exclude") or [])
    results = [
        fit_variant(variants[m], node, quant=quant, exclude=exclude)
        for m in wanted
        for node in (req.get("nodes") or [])
    ]
    return {"catalog_version": catalog.version, "results": results}
