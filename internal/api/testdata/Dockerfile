FROM debian:bookworm-slim
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends cloud-init \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/cloud/seed/nocloud \
    && printf 'datasource_list: [ NoCloud ]\nnetwork: {config: disabled}\ncloud_init_modules: [write-files]\n' \
        > /etc/cloud/cloud.cfg.d/99-seed-test.cfg
ENTRYPOINT ["/bin/sh", "-ec", "cloud-init init --local; cloud-init init; exec python3 -m http.server 8080 --bind 0.0.0.0 --directory /srv/seed-test"]
