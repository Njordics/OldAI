# Build environment for iso/build.sh (used automatically when the host has
# no xorriso). Go binaries are built on the host, so no Go toolchain here.
FROM ubuntu:24.04
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl git jq libgomp1 openssl unzip xorriso zstd \
 && rm -rf /var/lib/apt/lists/* \
 && git config --system --add safe.directory '*'
