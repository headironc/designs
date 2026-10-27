# This image is only for disposable local acceptance. It cannot produce hosted evidence.
ARG BASE
FROM ${BASE}
USER root
RUN apt-get update && apt-get install -y --no-install-recommends git procps sudo ca-certificates && rm -rf /var/lib/apt/lists/*
ENTRYPOINT []
