"""Loads and validates the model catalog (catalog/*.yaml)."""

from __future__ import annotations

import hashlib
import logging
import os
import re
from dataclasses import dataclass, field
from pathlib import Path

import yaml

log = logging.getLogger(__name__)

CAPABILITIES = {"chat", "code", "vision", "embeddings", "tools", "reasoning", "speech"}
RUNTIMES = {"ollama", "llamacpp", "vllm", "whisper"}
ID_RE = re.compile(r"^[a-z0-9][a-z0-9._:-]*$")

# Best first. Unknown quants sort after these.
QUANT_ORDER = ["bf16", "fp16", "f16", "q8_0", "q6_K", "q5_K_M", "q5_0", "awq", "gptq", "q4_K_M", "q4_0", "q3_K_M"]


def quant_rank(q: str) -> int:
    try:
        return QUANT_ORDER.index(q)
    except ValueError:
        return len(QUANT_ORDER)


class CatalogError(ValueError):
    pass


@dataclass
class Quant:
    quant: str
    runtime: str
    size: int
    ref: str = ""
    hf: str = ""
    revision: str = ""

    def to_json(self) -> dict:
        d = {"quant": self.quant, "runtime": self.runtime, "size": self.size}
        for k in ("ref", "hf", "revision"):
            if getattr(self, k):
                d[k] = getattr(self, k)
        return d


@dataclass
class Variant:
    id: str
    family_id: str
    params_b: float
    ctx_default: int
    arch: dict
    quants: list[Quant]  # best first

    def to_json(self) -> dict:
        return {
            "id": self.id,
            "params_b": self.params_b,
            "ctx_default": self.ctx_default,
            "arch": self.arch,
            "quants": [q.to_json() for q in self.quants],
        }


@dataclass
class Family:
    id: str
    family: str
    description: str
    license: dict
    capabilities: list[str]
    priority: int
    aliases: dict
    variants: list[Variant] = field(default_factory=list)

    def to_json(self) -> dict:
        return {
            "id": self.id,
            "family": self.family,
            "description": self.description,
            "license": self.license,
            "capabilities": self.capabilities,
            "priority": self.priority,
            "aliases": self.aliases,
            "variants": [v.to_json() for v in self.variants],
        }


@dataclass
class Catalog:
    families: list[Family]
    version: str

    def variants(self) -> dict[str, Variant]:
        return {v.id: v for f in self.families for v in f.variants}

    def family_of(self, variant_id: str) -> Family | None:
        for f in self.families:
            if any(v.id == variant_id for v in f.variants):
                return f
        return None

    def to_json(self) -> dict:
        return {"version": self.version, "families": [f.to_json() for f in self.families]}


def _req(d: dict, key: str, where: str):
    if key not in d:
        raise CatalogError(f"{where}: missing '{key}'")
    return d[key]


def parse_family(doc: dict, where: str) -> Family:
    if not isinstance(doc, dict):
        raise CatalogError(f"{where}: not a mapping")
    fid = str(_req(doc, "id", where))
    if not ID_RE.match(fid):
        raise CatalogError(f"{where}: bad id {fid!r}")
    lic = _req(doc, "license", where)
    if not isinstance(lic, dict) or "spdx" not in lic or "commercial_use" not in lic:
        raise CatalogError(f"{where}: license needs spdx and commercial_use")
    caps = list(_req(doc, "capabilities", where))
    bad = set(caps) - CAPABILITIES
    if bad or not caps:
        raise CatalogError(f"{where}: unknown capabilities {sorted(bad)}")

    fam = Family(
        id=fid,
        family=str(_req(doc, "family", where)),
        description=str(doc.get("description", "")),
        license=lic,
        capabilities=caps,
        priority=int(doc.get("priority", 50)),
        aliases=dict(doc.get("aliases") or {}),
    )
    for i, v in enumerate(_req(doc, "variants", where)):
        vw = f"{where} variant {i}"
        vid = str(_req(v, "id", vw))
        if not ID_RE.match(vid):
            raise CatalogError(f"{vw}: bad id {vid!r}")
        quants: list[Quant] = []
        for s in _req(v, "sources", vw):
            rt = _req(s, "runtime", vw)
            if rt not in RUNTIMES:
                raise CatalogError(f"{vw}: unknown runtime {rt!r}")
            for q in _req(s, "quants", vw):
                size = int(_req(q, "size", vw))
                if size <= 0:
                    raise CatalogError(f"{vw}: size must be positive")
                if rt == "ollama" and not q.get("ref"):
                    raise CatalogError(f"{vw}: ollama quant needs ref")
                quants.append(Quant(quant=str(_req(q, "quant", vw)), runtime=rt, size=size,
                                    ref=q.get("ref", ""), hf=q.get("hf", ""), revision=q.get("revision", "")))
        if not quants:
            raise CatalogError(f"{vw}: no quants")
        quants.sort(key=lambda q: quant_rank(q.quant))
        fam.variants.append(Variant(
            id=vid,
            family_id=fid,
            params_b=float(_req(v, "params_b", vw)),
            ctx_default=int(v.get("ctx_default", 4096)),
            arch=dict(v.get("arch") or {}),
            quants=quants,
        ))
    for alias, target in fam.aliases.items():
        if target not in {v.id for v in fam.variants}:
            raise CatalogError(f"{where}: alias {alias!r} points to unknown variant {target!r}")
    return fam


def load(dirs: list[Path]) -> Catalog:
    """Loads every *.yaml in dirs; later dirs override families with the same id."""
    by_id: dict[str, Family] = {}
    h = hashlib.sha256()
    for d in dirs:
        if not d.is_dir():
            continue
        for p in sorted(d.glob("*.yaml")):
            raw = p.read_bytes()
            h.update(p.name.encode() + b"\0" + raw)
            fam = parse_family(yaml.safe_load(raw), str(p))
            by_id[fam.id] = fam
    seen: dict[str, str] = {}
    for fam in by_id.values():
        for v in fam.variants:
            if v.id in seen:
                raise CatalogError(f"variant {v.id} defined in both {seen[v.id]} and {fam.id}")
            seen[v.id] = fam.id
    families = sorted(by_id.values(), key=lambda f: (-f.priority, f.family))
    return Catalog(families=families, version=h.hexdigest()[:16])


class CatalogStore:
    """Reloads the catalog when any file in its directories changes."""

    def __init__(self, dirs: list[Path]):
        self.dirs = dirs
        self._stamp = None
        self._catalog: Catalog | None = None
        self.error: str | None = None

    def _fingerprint(self):
        parts = []
        for d in self.dirs:
            if d.is_dir():
                for p in sorted(d.glob("*.yaml")):
                    st = p.stat()
                    parts.append((str(p), st.st_mtime_ns, st.st_size))
        return tuple(parts)

    def get(self) -> Catalog:
        stamp = self._fingerprint()
        if stamp != self._stamp or self._catalog is None:
            try:
                self._catalog = load(self.dirs)
                self.error = None
                log.info("catalog loaded: %d families, version %s", len(self._catalog.families), self._catalog.version)
            except (CatalogError, yaml.YAMLError, OSError) as e:
                # Keep serving the last good catalog; surface the error.
                self.error = str(e)
                log.error("catalog invalid, keeping previous: %s", e)
                if self._catalog is None:
                    raise
            self._stamp = stamp
        return self._catalog


def default_dirs() -> list[Path]:
    env = os.environ.get("AICLUSTER_CATALOG_DIRS")
    if env:
        return [Path(p) for p in env.split(os.pathsep) if p]
    return [Path("/usr/share/aicluster/catalog"), Path("/etc/aicluster/catalog.d")]
