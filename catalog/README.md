# Model catalog

One YAML file per model family. The Controller's provisioner loads every `*.yaml` here (and in `/etc/aicluster/catalog.d/` on an installed Controller, which overrides entries with the same `id`). `schema.json` is checked on load and in CI.

```yaml
id: qwen2.5                      # family id, unique
family: Qwen 2.5                 # display name
description: One line for the dashboard.
license: { spdx: Apache-2.0, name: Apache 2.0, commercial_use: true }
capabilities: [chat, code, tools]   # chat | code | vision | embeddings | tools | reasoning
priority: 80                     # higher = shown first, preferred by policies
variants:
  - id: qwen2.5:7b               # what clients put in "model" (plus aliases)
    params_b: 7.6
    arch: { layers: 28, kv_heads: 4, head_dim: 128, ctx_max: 32768 }   # optional; improves memory estimates
    ctx_default: 8192
    sources:
      - runtime: ollama
        quants:
          - { quant: q4_K_M, ref: library/qwen2.5:7b-instruct-q4_K_M, size: 4683087232 }
aliases: { qwen: qwen2.5:7b }
```

`size` is the download size in bytes as published by the source. The Controller replaces it with the exact size from the manifest once it has downloaded the model. Memory needed to run a model is estimated from `size`, `arch` and the context length; see `controller/provisioner`.

Not yet in the catalog: Whisper (needs the separate speech runtime) and vLLM/llama.cpp sources (Hugging Face), which arrive with those runtimes.
