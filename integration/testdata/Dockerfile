ARG BASE
FROM ${BASE}
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends qemu-utils diffutils \
 && rm -rf /var/lib/apt/lists/*
CMD ["sleep", "infinity"]
