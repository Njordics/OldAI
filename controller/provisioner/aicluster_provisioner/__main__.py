"""aicluster-provisioner: serve the catalog and fit API.

    python -m aicluster_provisioner --listen unix:/run/aicluster/provisioner.sock --catalog /usr/share/aicluster/catalog
    python -m aicluster_provisioner --listen tcp:127.0.0.1:7460 --catalog ./catalog   # development
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from pathlib import Path

from .catalog import CatalogStore, default_dirs
from .server import bound_address, make_server


class JSONFormatter(logging.Formatter):
    def format(self, record):
        out = {"time": self.formatTime(record), "level": record.levelname, "msg": record.getMessage(), "logger": record.name}
        if record.exc_info:
            out["exc"] = self.formatException(record.exc_info)
        return json.dumps(out)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="aicluster-provisioner")
    ap.add_argument("--listen", default="unix:/run/aicluster/provisioner.sock")
    ap.add_argument("--catalog", action="append", type=Path, help="catalog directory (repeatable; later overrides earlier)")
    ap.add_argument("--log-level", default="info")
    args = ap.parse_args(argv)

    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(JSONFormatter())
    logging.basicConfig(level=args.log_level.upper(), handlers=[handler])

    store = CatalogStore(args.catalog or default_dirs())
    store.get()  # fail fast on a broken catalog at startup
    srv = make_server(args.listen, store)
    logging.getLogger("aicluster_provisioner").info("listening on %s", bound_address(srv))
    print(f"READY {bound_address(srv)}", flush=True)  # for supervisors/tests that start us
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
