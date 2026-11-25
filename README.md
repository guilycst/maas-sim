# maas-sim

maas-sim is a standalone test simulator for a subset of the Canonical MAAS v2 API. It models machine allocation, deployment, power and release, along with zones, resource pools, tags, subnets, IP reservations and DNS. Run it in memory for API tests or use Docker to start containers for deployed machines.

See the [compatibility matrix](COMPATIBILITY.md) for supported methods, parameters and test coverage. The version endpoint returns a fixture payload. Its version and capability strings do not describe the simulator's support.

## Run the container image

Run this command from a checkout containing `examples/seed.yaml`.

```sh
docker run --rm -p 127.0.0.1:5240:5240 \
  --mount type=bind,src="$PWD/examples/seed.yaml",dst=/seed.yaml,readonly \
  ghcr.io/guilycst/maas-sim:v0.0.1 --seed=/seed.yaml
```

The image listens on all container interfaces. Publish the HTTP port on host loopback for local tests. Docker mode also needs access to a Docker daemon. Use it with trusted node images and bootstrap data on an isolated test host.

If a registry pull is unavailable, download the archive for your platform from the [GitHub release](https://github.com/guilycst/maas-sim/releases/tag/v0.0.1). Load it with `docker load --input maas-sim-v0.0.1-linux-amd64.tar.gz`, using the `arm64` archive on ARM hosts, then run the same command above.

## Start in memory

Requires Go 1.23 or later.

```sh
go run ./cmd/maas-sim --seed examples/seed.yaml
```

Connect clients to `http://127.0.0.1:5240/MAAS` with API version `2.0`. The OAuth 1.0a key format is `<consumer_key>:<token_key>:<token_secret>`. Use `abc:def:ghi` for tests.

```sh
curl -fsS -H 'Authorization: OAuth oauth_consumer_key="abc", oauth_token="def"' \
  'http://127.0.0.1:5240/MAAS/api/2.0/machines/?status=4'
```

The listener defaults to loopback. The simulator checks OAuth header syntax and maps every accepted request to the same fixture user, `admin`. It does not verify credentials or signatures. Use an isolated test network, particularly when using Docker with privileged containers.

## Seed the inventory

Pass one YAML or JSON document to `--seed`. Both formats use the fields in [the example seed](examples/seed.yaml). `schemaVersion: 1` versions the seed file, and `/MAAS/api/2.0/` identifies the HTTP API.

| Field | Meaning |
| --- | --- |
| `domain` | DNS domain, default `maas`. |
| `zones`, `resourcePools` | Lists of resource names. Each defaults to `[default]` when omitted. |
| `tags` | List of tag names, empty by default. |
| `machines` | Required machine list. An empty list creates an empty inventory. |
| `machines[].hostname` | Required, unique short DNS name. The loader normalizes it to lowercase. |
| `machines[].systemID` | Optional, unique ID. A supplied value uses lowercase letters, digits and hyphens, starts with a letter or digit, and fits within 41 characters. |
| `machines[].zone`, `machines[].resourcePool`, `machines[].tags` | References to the declared names. A machine with an omitted zone or resource pool uses `default`. |
| `machines[].cpuCount`, `machines[].memoryMiB`, `machines[].diskGiB` | Positive integers describing the reported capacity of the machine. |

An omitted `systemID` becomes the first 12 lowercase hex characters of SHA-256 of the normalized hostname. For example, `lab-node-01` yields `ba6f8fd121d8`. Use an explicit ID to preserve identity across hostname changes in the seed file. Renaming a machine through the API preserves its ID during the current run.

Machines start `Ready`, powered off, without owners or deployment IPs. Seed errors, including duplicate fields, unknown names and conflicting IDs, fail startup. Restarting reloads the initial inventory. Omit `--seed` to use six built-in machines or set `--machines` to select a different count.

## Deploy Docker containers

Pass the node image and, optionally, a user-defined Docker bridge network. The image must have a running entrypoint and a directory at `/var/lib/cloud/seed/nocloud`. To execute bootstrap data, supply an image configured for the NoCloud datasource. The simulator writes `user-data` and `meta-data` into a Docker volume before starting the container.

```sh
go run ./cmd/maas-sim --mode=docker --seed examples/seed.yaml \
  --node-image="$NODE_IMAGE" --docker-network=maas-sim
```

The MAAS subnet follows the Docker bridge. The simulator learns the container's IPv4 address from Docker and returns it in the machine and interface records. Use `--docker-subnet` or a bridge created with an explicit subnet if the test needs reservations for specific unused IPs. Docker holds reserved addresses with lightweight containers. The DNS listener serves machine IPs and each record's configured addresses.

| Flag | Default |
| --- | --- |
| `--mode` | `memory` |
| `--listen` | `127.0.0.1` |
| `--port` | `5240` |
| `--dns-port` | `1053` over UDP and TCP. Use `0` to disable DNS. |
| `--deploy-delay` | `200ms` in memory mode |
| `--docker-network` | `maas-sim` |
| `--cleanup` | `true` |

For custom node runtimes, `--node-privileged`, `--node-cgroupns`, repeatable `--node-bind` and repeatable `--node-tmpfs` configure the Docker containers. The Docker client's environment includes `DOCKER_HOST`, which defaults to the local Unix socket on Linux. With cleanup enabled, SIGINT and SIGTERM remove simulator-owned containers and volumes. A bridge created by the simulator is also removed.

## Run the tests

```sh
go test -race ./...
docker build -t maas-sim-test-node:local internal/api/testdata
MAAS_SIM_TEST_DOCKER=1 go test -run TestDockerNodeLifecycle -v ./internal/api
```

The tests use the official Canonical client. The Docker test requires a Linux Docker bridge reachable from the test process and verifies a real cloud-init NoCloud boot alongside DNS and IP reservation behavior.
