# MAAS API compatibility

maas-sim implements the operations below for integration tests. The HTTP API uses the `/MAAS/api/2.0/` prefix. For a listed operation, `?op=NAME` and an `op-NAME` path suffix select the same handler. For the upstream contract, see the [Canonical MAAS API v2 reference](https://canonical.com/maas/docs/stable/reference/api-reference/api-v2-generated/).

`GET version/` returns the fixture version `3.5.0` and advertises `static-ipaddresses`. Use this matrix as the support list. The simulator has no release-level MAAS conformance claim, and some fields outside the documented subset are silently ignored.

**Tested** means the repository's test suite exercises the listed path or behavior, using Canonical's Go client v0.20.0 or a direct request. **Docker tested** refers to the opt-in [Docker lifecycle test](internal/api/api_test.go). **Implemented** means the handler or variant is present in source and has no dedicated assertion in the suite. A test for a listed operation may cover only the inputs noted in its row.

## Machines

| Method | Path | Operation | Behavior and limits | Coverage |
| --- | --- | --- | --- | --- |
| `GET` | `machines/` | List | Matches hostname or FQDN, system ID, status, zone, pool, required and excluded tags, and minimum CPU and memory. | Tested for HTTP 200 with hostname, status and zone. |
| `GET` | `machines/` | `list_allocated` | Lists machines in `Allocated`, `Deploying` or `Deployed` across the simulated inventory. | Implemented. |
| `POST` | `machines/` | `allocate` | Selects the first matching `Ready` machine. Uses the list filters and accepts `dry_run=true` to leave state untouched. Returns HTTP 409 when no machine matches. Disk size and architecture are reported fields and do not affect selection. | Tested for tags, zone, pool, CPU, memory, conflicts and concurrent allocation. Dry run is implemented. |
| `GET` | `machines/{system_id}/` | Read | Returns a machine by system ID, including the recorded status, interface and deployment IP. | Tested, including a seed-supplied ID. Docker tested. |
| `PUT` | `machines/{system_id}/` | Update | Renames a `Ready` or `Allocated` machine without changing its ID. The new hostname must be unique and produce a valid FQDN. Accepts `swap_size` as metadata. | Rename and invalid names tested. Swap metadata implemented. |
| `GET` | `machines/{system_id}/interfaces/` | List | Returns one synthetic boot interface. After deployment it includes the backend's IPv4 address and, in Docker mode, the observed MAC address. | Endpoint implemented. The embedded interface IP is tested through the machine response. |
| `POST`, `PUT` | `machines/{system_id}/` | `deploy` | Transitions an allocated machine through `Deploying` to `Deployed` or `Failed deployment`. Accepts standard base64 `user_data`. Stores `distro_series`, `osystem`, `hwe_kernel` and `ephemeral_deploy` as metadata. | POST tested for successful deployment, distro metadata and multipart input. Docker tested for NoCloud boot. PUT and the failure state are implemented. |
| `POST`, `PUT` | `machines/{system_id}/` | `release` | Returns a machine to `Ready`, clears its owner and deployment IP, restores its seed tags and detaches the released IP from explicit DNS resources. An in-flight deployment can be cancelled through release. | POST tested, including cancellation. Docker tested for release. PUT implemented. |
| `POST`, `PUT` | `machines/{system_id}/` | `power_on`, `power_off` | Changes power state for deployed machines. Memory mode changes the recorded state. Docker starts or stops the container, and the bridge IP can change after restart. | POST tested in memory. PUT and Docker power transitions are implemented. |
| `GET`, `POST`, `PUT` | `machines/{system_id}/` | `query_power_state` | Reads the simulator's recorded state. | GET tested. POST and PUT implemented. |

Machine registration, commissioning, hardware testing, rescue mode, locking, storage layout and disk erasure are outside this subset.

## Inventory and identity

| Method | Path | Operation | Behavior and limits | Coverage |
| --- | --- | --- | --- | --- |
| `GET` | `version/` | Read | Returns the fixed version payload described above. | Tested. |
| `GET` | `users/` | `whoami` | Every accepted OAuth header maps to the same fixture superuser, `admin`. | Tested. |
| `GET` | `domains/` | List | Returns the single seeded or default domain. | Tested. |
| `GET` | `domains/{id}/` | Read | Reads the only domain by ID. | Implemented. |
| `PUT`, `POST` | `domains/{id}/` | Update | Changes the domain's name, TTL or authoritative flag in the API. A rename also updates machine FQDNs. The authoritative flag is stored as metadata. | PUT tested. POST implemented. |
| `POST`, `PUT` | `domains/{id}/` | `set_default` | Returns the single domain, which is already the default. | Implemented. |
| `GET` | `zones/`, `resourcepools/` | List | Returns names and IDs from the initial inventory. These collections are read-only through the API. | Tested. |
| `GET` | `tags/`, `tags/{name}/` | List, read | Returns tag definitions stored in memory. | Implemented. |
| `POST` | `tags/` | Create | Creates a tag using the seed's name rules. The definition, comment and kernel options are metadata. Tag expressions are unevaluated. | Creation and invalid names tested. |
| `GET` | `tags/{name}/` | `machines` | Lists the machines carrying the tag. | Implemented. |
| `POST` | `tags/{name}/` | `update_nodes` | Adds or removes tag membership by machine ID. | Add tested, including subsequent allocation by tag. Remove implemented. |

HTTP requests use a syntactically valid OAuth header with nonempty consumer and token fields. Signature verification, credential lookup, per-user isolation and authorization are outside the simulator's scope. The suite checks that malformed headers are rejected. The API has no user management or discovery endpoint.

## Network and DNS

| Method | Path | Operation | Behavior and limits | Coverage |
| --- | --- | --- | --- | --- |
| `GET` | `fabrics/`, `fabrics/1/vlans/` | List | Exposes one synthetic fabric and one untagged VLAN. | Tested. |
| `GET` | `vlans/`, `subnets/`, `ipranges/` | List | Exposes the same VLAN, one IPv4 subnet and one synthetic dynamic range. In Docker mode they describe the selected bridge. | Implemented. |
| `GET` | `ipaddresses/` | List | Returns reservations and deployment addresses, with optional `subnet`, `ip` and `owner` filters. | Empty inventory after cancellation tested. Filters implemented. |
| `POST` | `ipaddresses/` | `reserve`, `release` | Reserves a requested IPv4 address or picks one from the subnet. A new Docker lease uses a lightweight container. Reserving a deployed machine's IP records a reservation without a second container. Release removes the reservation and any lease container. | Automatic reservation and release tested in both modes. Explicit address and hostname variants implemented. |
| `GET` | `dnsresources/` | List | Filters explicit resources by FQDN, name or the single configured domain. | FQDN query tested. Name and domain filters implemented. |
| `POST` | `dnsresources/` | Create | Creates a resource by `fqdn` or `name` and `domain`. Supports a TTL and zero or more literal IPv4 addresses. | FQDN creation tested in both modes, including an empty resource in memory mode. |
| `GET` | `dnsresources/{id}/` | Read | Returns the explicit DNS resource by ID. | Tested. |
| `PUT`, `POST` | `dnsresources/{id}/` | Update | Updates the name, TTL or addresses. An empty `ip_addresses` clears the addresses. The response omits `ip_addresses` when the list is empty. | PUT with an IPv4 address tested. POST and explicit empty updates implemented. |
| `DELETE` | `dnsresources/{id}/` | Delete | Deletes a record by path ID and returns HTTP 204. | Tested in both modes. |
| `DELETE` | `dnsresources/` | Delete | Also accepts an `id` or `fqdn` parameter on the collection. | Implemented. |

The simulator models one IPv4 network with read-only fabric, VLAN, subnet, range and interface topology. DHCP, IPv6 deployment, and networking changes such as creating VLANs or attaching interfaces are outside this subset.

## Runtime features

| Feature | Behavior and limits | Coverage |
| --- | --- | --- |
| Declarative inventory | `--seed` loads a single YAML or JSON document with `schemaVersion: 1`. Machines start `Ready`. The simulator derives an omitted `systemID` from the normalized hostname. Runtime changes are held in memory and restart reloads the initial inventory. | YAML/JSON equivalence, ID handling and invalid seeds tested. |
| Memory backend | Assigns synthetic IPv4 addresses in `192.0.2.0/24`, simulates deployment delay and does not start a workload. | Tested. |
| Docker backend | Starts the supplied `--node-image` on a user-defined bridge and provides NoCloud data before boot. `Deployed` indicates that Docker started the container and reported its IPv4 address. The image is responsible for executing bootstrap data. There is no OS installation, PXE, BMC or hypervisor integration. | Docker tested with Debian and cloud-init. |
| DNS listener | Serves authoritative IPv4 `A` answers for explicit DNS resources, named reservations and machines. The CLI listens over UDP and TCP. This is an authoritative lookup service with no recursion, other record types, or proxying to the application at the advertised address. | UDP answers for explicit resources tested in both modes. TCP and other name sources are implemented. |
| Docker cleanup | Machine release removes its container and attached anonymous volumes. With `--cleanup` enabled, graceful shutdown removes simulator-owned containers and any bridge it created. | Release is exercised by the Docker test. Graceful shutdown cleanup is implemented. |

See [Run the tests](README.md#run-the-tests) for the standard and Docker suites.
