import json
import threading
import urllib.request
from pathlib import Path

import pytest
import yaml

from aicluster_provisioner import catalog as cat
from aicluster_provisioner import fit
from aicluster_provisioner.server import bound_address, make_server

REPO = Path(__file__).resolve().parents[3]
CATALOG = REPO / "catalog"
GiB = 1 << 30


@pytest.fixture(scope="module")
def catalog():
    return cat.load([CATALOG])


def node(**kw):
    n = {"node_id": "n1", "gpus": [], "ram_total": 16 * GiB, "disk_free": 500 * GiB, "committed": []}
    n.update(kw)
    return n


def test_repo_catalog_loads(catalog):
    ids = catalog.variants()
    assert "qwen2.5:7b" in ids and "nomic-embed-text:v1.5" in ids
    for v in ids.values():
        assert [q.quant for q in v.quants] == sorted((q.quant for q in v.quants), key=cat.quant_rank)


def test_repo_catalog_matches_schema():
    jsonschema = pytest.importorskip("jsonschema")
    schema = json.loads((CATALOG / "schema.json").read_text())
    for p in CATALOG.glob("*.yaml"):
        jsonschema.validate(yaml.safe_load(p.read_text()), schema)


def test_rejects_bad_family(tmp_path):
    (tmp_path / "bad.yaml").write_text("id: x\nfamily: X\nlicense: {spdx: MIT, commercial_use: true}\ncapabilities: [telepathy]\nvariants: []\n")
    with pytest.raises(cat.CatalogError, match="capabilities"):
        cat.load([tmp_path])


def test_override_dir_replaces_family(tmp_path):
    (tmp_path / "mistral.yaml").write_text(
        "id: mistral\nfamily: Mistral (local)\nlicense: {spdx: Apache-2.0, commercial_use: true}\n"
        "capabilities: [chat]\nvariants:\n  - id: mistral:7b\n    params_b: 7\n    sources:\n"
        "      - runtime: ollama\n        quants: [{quant: q4_K_M, ref: library/mistral:7b, size: 100}]\n")
    c = cat.load([CATALOG, tmp_path])
    assert c.family_of("mistral:7b").family == "Mistral (local)"


def test_gpu_node_gets_best_quant_that_fits_vram(catalog):
    v = catalog.variants()["qwen2.5:7b"]
    r = fit.fit_variant(v, node(gpus=[{"vram": 24 * GiB}]))
    assert r["fits"] and r["placement"] == "gpu" and r["quant"] == "q8_0"
    r = fit.fit_variant(v, node(gpus=[{"vram": 8 * GiB}]))
    assert r["placement"] == "gpu" and r["quant"] in ("q5_K_M", "q4_K_M")


def test_gpu_beats_quality(catalog):
    # 8 GB card: q8_0 (8 GB) only fits on CPU, q4_K_M fits on GPU -> GPU wins.
    v = catalog.variants()["qwen2.5:7b"]
    r = fit.fit_variant(v, node(gpus=[{"vram": 6 * GiB}], ram_total=64 * GiB))
    assert r["placement"] == "gpu" and r["quant"] == "q4_K_M"


def test_hybrid_and_cpu_fallback(catalog):
    v = catalog.variants()["qwen2.5:14b"]
    r = fit.fit_variant(v, node(gpus=[{"vram": 8 * GiB}], ram_total=32 * GiB))
    assert r["placement"] == "hybrid"
    r = fit.fit_variant(v, node(ram_total=32 * GiB))
    assert r["placement"] == "cpu"


def test_too_big(catalog):
    r = fit.fit_variant(catalog.variants()["llama3.1:70b"], node(ram_total=16 * GiB))
    assert not r["fits"] and "memory" in r["reason"]


def test_committed_models_reduce_budget_and_exclude_frees(catalog):
    v = catalog.variants()["qwen2.5:7b"]
    busy = node(gpus=[{"vram": 12 * GiB}], committed=[{"id": "a1", "vram": 10 * GiB, "residency": "resident", "size": 5 * GiB, "downloaded": True}])
    assert fit.fit_variant(v, busy)["placement"] != "gpu"
    assert fit.fit_variant(v, busy, exclude={"a1"})["placement"] == "gpu"  # swap frees it
    on_demand = node(gpus=[{"vram": 12 * GiB}], committed=[{"id": "a2", "vram": 10 * GiB, "residency": "on_demand"}])
    assert fit.fit_variant(v, on_demand)["placement"] == "gpu"


def test_disk_limit(catalog):
    r = fit.fit_variant(catalog.variants()["qwen2.5:7b"], node(disk_free=6 * GiB))
    assert not r["fits"] and "disk" in r["reason"]


def test_forced_quant(catalog):
    r = fit.fit_variant(catalog.variants()["qwen2.5:7b"], node(ram_total=64 * GiB), quant="q4_K_M")
    assert r["quant"] == "q4_K_M" and len(r["options"]) == 1


def test_http_api(tmp_path):
    srv = make_server("tcp:127.0.0.1:0", cat.CatalogStore([CATALOG]))
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    base = "http://" + bound_address(srv).removeprefix("tcp:")
    try:
        with urllib.request.urlopen(base + "/catalog") as r:
            body = json.load(r)
        assert body["families"] and body["version"]
        req = urllib.request.Request(base + "/fit", method="POST",
                                     data=json.dumps({"nodes": [node()], "models": ["qwen2.5:0.5b"]}).encode())
        with urllib.request.urlopen(req) as r:
            res = json.load(r)["results"]
        assert res[0]["fits"] and res[0]["placement"] == "cpu"
        bad = urllib.request.Request(base + "/fit", method="POST", data=json.dumps({"nodes": [node()], "models": ["nope"]}).encode())
        with pytest.raises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(bad)
        assert e.value.code == 404
    finally:
        srv.shutdown()


def test_null_lists_from_go(catalog):
    # Go encodes empty slices as null.
    r = fit.fit_variant(catalog.variants()["qwen2.5:0.5b"], {"node_id": "n", "gpus": None, "committed": None, "ram_total": 8 * GiB, "disk_free": None})
    assert r["fits"]
    assert fit.fit(catalog, {"nodes": None})["results"] == []
