import pytest

from aicluster_provisioner import catalog as cat
from aicluster_provisioner import fit
from aicluster_provisioner.pool import pool_variant

from test_provisioner import CATALOG, GiB, node


@pytest.fixture(scope="module")
def variants():
    return cat.load([CATALOG]).variants()


def nodes(*specs, pool_gpu=True):
    """Nodes whose llama.cpp can use their GPUs, unless pool_gpu=False."""
    return [node(node_id=f"n{i}", pool_gpu=pool_gpu, **s) for i, s in enumerate(specs)]


def test_gpu_memory_ignored_when_pooled_llamacpp_is_cpu_only(variants):
    # Big cards, little RAM: the CPU-only llama.cpp would hold its share in
    # RAM, so the cards' memory must not count.
    ns = nodes(*[dict(gpus=[{"vram": 24 * GiB}], ram_total=16 * GiB)] * 3, pool_gpu=False)
    r = pool_variant(variants["llama3.1:70b"], ns)
    assert not r["fits"]
    ns = nodes(*[dict(gpus=[{"vram": 24 * GiB}], ram_total=64 * GiB)] * 2, pool_gpu=False)
    r = pool_variant(variants["llama3.1:70b"], ns)
    assert r["fits"]
    for m in r["members"]:
        assert m["placement"] == "cpu" and m["vram"] == 0 and m["ram"] > 0


def test_gpu_pool_for_model_too_big_for_any_card(variants):
    # 70B q4 (~45 GB) on three 24 GB cards: no single node fits, two or three do.
    ns = nodes(*[dict(gpus=[{"vram": 24 * GiB}], ram_total=16 * GiB)] * 3)
    assert not any(fit.fit_variant(variants["llama3.1:70b"], n)["fits"] for n in ns)
    r = pool_variant(variants["llama3.1:70b"], ns)
    assert r["fits"] and r["pooled"]
    assert all(m["placement"] == "gpu" for m in r["members"])
    assert len(r["members"]) in (2, 3)
    assert abs(sum(m["share"] for m in r["members"]) - 1) < 0.01
    assert sum(m["leader"] for m in r["members"]) == 1


def test_falls_back_to_ram_when_gpus_are_not_enough(variants):
    ns = nodes(dict(gpus=[{"vram": 8 * GiB}], ram_total=32 * GiB), dict(ram_total=32 * GiB), dict(ram_total=32 * GiB))
    r = pool_variant(variants["llama3.1:70b"], ns)
    assert r["fits"] and any(m["placement"] != "gpu" for m in r["members"])


def test_respects_member_limit_and_reports_shortfall(variants):
    ns = nodes(*[dict(ram_total=8 * GiB)] * 6)
    assert not pool_variant(variants["llama3.1:70b"], ns, max_nodes=3)["fits"]
    r = pool_variant(variants["llama3.1:70b"], ns, max_nodes=3)
    assert "not enough memory" in r["reason"]


def test_explicit_uses_every_chosen_node_in_proportion(variants):
    ns = nodes(dict(ram_total=96 * GiB), dict(ram_total=32 * GiB))
    r = pool_variant(variants["qwen2.5:7b"], ns, explicit=True)
    assert r["fits"] and r["pooled"] and len(r["members"]) == 2
    shares = {m["node_id"]: m["share"] for m in r["members"]}
    assert shares["n0"] > shares["n1"] > 0  # bigger node, bigger share
    assert next(m for m in r["members"] if m["leader"])["node_id"] == "n0"


def test_explicit_gpu_split_when_cards_suffice(variants):
    # 70B q4 needs ~48 GB with its KV cache: two 24 GB cards (~43 GB usable)
    # fall back to GPU + RAM, three hold it on GPUs alone, in equal parts.
    two = pool_variant(variants["llama3.1:70b"], nodes(*[dict(gpus=[{"vram": 24 * GiB}])] * 2), explicit=True)
    assert two["fits"] and any(m["placement"] != "gpu" for m in two["members"])
    r = pool_variant(variants["llama3.1:70b"], nodes(*[dict(gpus=[{"vram": 24 * GiB}])] * 3), explicit=True)
    assert r["fits"] and all(m["placement"] == "gpu" for m in r["members"])
    assert all(abs(m["share"] - 1 / 3) < 0.01 for m in r["members"])


def test_leader_needs_disk_for_the_file(variants):
    # n0 has the biggest GPU (largest share) but no room for the 42 GB file.
    ns = nodes(dict(gpus=[{"vram": 40 * GiB}], disk_free=8 * GiB), dict(gpus=[{"vram": 24 * GiB}], disk_free=500 * GiB))
    r = pool_variant(variants["llama3.1:70b"], ns)
    assert r["fits"]
    shares = {m["node_id"]: m["share"] for m in r["members"]}
    assert shares["n0"] > shares["n1"]
    assert next(m for m in r["members"] if m["leader"])["node_id"] == "n1"


def test_fit_suggests_pools_only_when_no_single_node_fits(variants):
    c = cat.load([CATALOG])
    ns = nodes(*[dict(gpus=[{"vram": 24 * GiB}], ram_total=16 * GiB)] * 3)
    out = fit.fit(c, {"nodes": ns, "models": ["qwen2.5:7b", "llama3.1:70b"], "pool": True})
    assert [p["model_id"] for p in out["pools"]] == ["llama3.1:70b"]
    assert out["pools"][0]["fits"]


def test_pool_endpoint_function(variants):
    c = cat.load([CATALOG])
    ns = nodes(dict(ram_total=48 * GiB), dict(ram_total=48 * GiB))
    out = fit.pool(c, {"model": "llama3.1:70b", "nodes": ns, "explicit": True})
    assert out["result"]["fits"] and out["result"]["runtime"] == "llamacpp"
    with pytest.raises(KeyError):
        fit.pool(c, {"model": "nope", "nodes": ns})
