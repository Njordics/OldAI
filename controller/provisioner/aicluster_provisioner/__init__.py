"""Model catalog and placement engine for the aicluster Controller.

The Go core owns cluster state; this service only answers questions about
models: what the catalog contains and which quantisation of a model fits on
which node (docs/architecture.md section 3, "Go <-> Python boundary").
"""
