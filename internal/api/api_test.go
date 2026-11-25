package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	maas "github.com/canonical/gomaasclient/client"
	"github.com/canonical/gomaasclient/entity"
	"github.com/canonical/gomaasclient/entity/node"
	"github.com/miekg/dns"

	"github.com/guilycst/maas-sim/internal/api"
	"github.com/guilycst/maas-sim/internal/driver"
	"github.com/guilycst/maas-sim/internal/seed"
	"github.com/guilycst/maas-sim/internal/store"
)

const authHeader = `OAuth oauth_consumer_key="abc", oauth_token="def"`

func start(t *testing.T, backend driver.MachineDriver, inventory seed.Inventory) (*httptest.Server, *api.Server, *maas.Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	handler := api.New(ctx, store.New(inventory, backend.Network()), backend)
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		cancel()
		handler.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := backend.Cleanup(ctx); err != nil {
			t.Errorf("cleanup backend: %v", err)
		}
	})
	client, err := maas.GetClient(server.URL+"/MAAS", "abc:def:ghi", "2.0")
	if err != nil {
		t.Fatal(err)
	}
	return server, handler, client
}

func TestCanonicalClientSeed(t *testing.T) {
	inventory, err := seed.Load("../../examples/seed.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, _, client := start(t, driver.NewMemory(0), inventory)
	domains, err := client.Domains.Get()
	if err != nil || len(domains) != 1 || domains[0].Name != "lab.test" {
		t.Fatalf("seeded domains: %+v, %v", domains, err)
	}
	pools, err := client.ResourcePools.Get()
	if err != nil || len(pools) != 1 || pools[0].Name != "lab" {
		t.Fatalf("seeded pools: %+v, %v", pools, err)
	}

	machine, err := client.Machines.Allocate(&entity.MachineAllocateParams{
		Tags: []string{"small"}, Pool: "lab", Zone: "az1", CPUCount: 4, Mem: 8192,
	})
	if err != nil || machine.SystemID != "ba6f8fd121d8" || machine.Status != node.StatusAllocated {
		t.Fatalf("allocate seeded machine: %+v, %v", machine, err)
	}
	if machine.FQDN != "lab-node-01.lab.test" || machine.Storage != 80*(1<<30)/1e6 {
		t.Fatalf("seeded FQDN = %q, storage = %v MB", machine.FQDN, machine.Storage)
	}
	machine, err = client.Machine.Update(machine.SystemID, &entity.MachineUpdateParams{
		Hostname: "renamed-node",
	}, nil)
	if err != nil || machine.SystemID != "ba6f8fd121d8" || machine.FQDN != "renamed-node.lab.test" {
		t.Fatalf("rename seeded machine: %+v, %v", machine, err)
	}
	machine, err = client.Machine.Release(machine.SystemID, &entity.MachineReleaseParams{})
	if err != nil || machine.Status != node.StatusReady || machine.SystemID != "ba6f8fd121d8" {
		t.Fatalf("release seeded machine: %+v, %v", machine, err)
	}
	explicit, err := client.Machine.Get("lab002")
	if err != nil || explicit.Hostname != "lab-node-02" || explicit.CPUCount != 8 || explicit.Memory != 16384 {
		t.Fatalf("get machine with explicit ID: %+v, %v", explicit, err)
	}
}

func TestOperationPathsPreserveResourceNames(t *testing.T) {
	inventory := seed.Default(1)
	inventory.Machines[0].SystemID = "op-allocate"
	inventory.Tags = append(inventory.Tags, "op-team")
	server, _, client := start(t, driver.NewMemory(0), inventory)
	machine, err := client.Machine.Get("op-allocate")
	if err != nil || machine.SystemID != "op-allocate" {
		t.Fatalf("get machine with operation-like ID: %+v, %v", machine, err)
	}
	tag, err := client.Tag.Get("op-team")
	if err != nil || tag.Name != "op-team" {
		t.Fatalf("get tag with operation-like name: %+v, %v", tag, err)
	}
	expectStatus(t, server, http.MethodPost, store.Prefix+"machines/op-allocate/", http.StatusOK)
	expectStatus(t, server, http.MethodPost, store.Prefix+"machines/op-allocate/?op=release", http.StatusOK)
}

func TestRejectDuplicateMachineHostname(t *testing.T) {
	_, _, client := start(t, driver.NewMemory(0), seed.Default(2))
	_, err := client.Machine.Update("sim001", &entity.MachineUpdateParams{Hostname: "node-002"}, nil)
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("rename to duplicate hostname: %v", err)
	}
	machine, err := client.Machine.Get("sim001")
	if err != nil || machine.Hostname != "node-001" {
		t.Fatalf("get machine after conflicting rename: %+v, %v", machine, err)
	}
}

func TestRejectInvalidInventoryNames(t *testing.T) {
	inventory := seed.Default(1)
	inventory.Domain = strings.Repeat("a.", 120) + "test"
	inventory.Machines[0].Hostname = "node001"
	_, _, client := start(t, driver.NewMemory(0), inventory)
	for name, change := range map[string]func() error{
		"tag path": func() error {
			_, err := client.Tags.Create(&entity.TagParams{Name: "qa/team"})
			return err
		},
		"machine FQDN": func() error {
			_, err := client.Machine.Update("sim001", &entity.MachineUpdateParams{Hostname: strings.Repeat("a", 63)}, nil)
			return err
		},
		"domain FQDN": func() error {
			_, err := client.Domain.Update(1, &entity.DomainParams{Name: strings.Repeat("a.", 124) + "abcde"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := change(); err == nil || !strings.Contains(err.Error(), "400") {
				t.Fatalf("invalid name returned %v", err)
			}
		})
	}
}

func TestCanonicalClientLifecycle(t *testing.T) {
	server, handler, client := start(t, driver.NewMemory(40*time.Millisecond), seed.Default(2))
	version, err := client.Version.Get()
	if err != nil || version.Version != "3.5.0" || len(version.Capabilities) != 1 || version.Capabilities[0] != "static-ipaddresses" {
		t.Fatalf("get version: %+v, %v", version, err)
	}
	user, err := client.Users.Whoami()
	if err != nil || !user.IsSuperUser || user.UserName != "admin" {
		t.Fatalf("get admin: %+v, %v", user, err)
	}
	domains, err := client.Domains.Get()
	if err != nil || len(domains) != 1 || domains[0].Name != "maas" {
		t.Fatalf("list domains: %+v, %v", domains, err)
	}
	updated, err := client.Domain.Update(domains[0].ID, &entity.DomainParams{
		Name: "lab.test", TTL: 30, Authoritative: true,
	})
	if err != nil || updated.Name != "lab.test" || updated.TTL != 30 {
		t.Fatalf("update domain: %+v, %v", updated, err)
	}
	fabrics, err := client.Fabrics.Get()
	if err != nil || len(fabrics) != 1 {
		t.Fatalf("list fabrics: %+v, %v", fabrics, err)
	}
	vlans, err := client.VLANs.Get(fabrics[0].ID)
	if err != nil || len(vlans) != 1 || vlans[0].VID != 0 {
		t.Fatalf("list vlans: %+v, %v", vlans, err)
	}
	zones, err := client.Zones.Get()
	if err != nil || len(zones) != 3 || zones[1].Name != "az1" {
		t.Fatalf("list zones: %+v, %v", zones, err)
	}
	pools, err := client.ResourcePools.Get()
	if err != nil || len(pools) != 1 || pools[0].Name != "default" {
		t.Fatalf("list pools: %+v, %v", pools, err)
	}

	tag, err := client.Tags.Create(&entity.TagParams{Name: "integration"})
	if err != nil || tag.Name != "integration" {
		t.Fatalf("create tag: %+v, %v", tag, err)
	}
	if err := client.Tag.AddMachines(tag.Name, []string{"sim002"}); err != nil {
		t.Fatal(err)
	}

	machine, err := client.Machines.Allocate(&entity.MachineAllocateParams{
		Tags: []string{"worker", "integration"}, Zone: "az1", Pool: "default", CPUCount: 8, Mem: 16384,
	})
	if err != nil || machine.Status != node.StatusAllocated || machine.SystemID != "sim002" {
		t.Fatalf("allocate machine: %+v, %v", machine, err)
	}
	operation := store.Prefix + "machines/" + machine.SystemID + "/"
	expectStatus(t, server, http.MethodGet, store.Prefix+"machines/?hostname=node-002&status=10&zone=az1", http.StatusOK)
	expectStatus(t, server, http.MethodPost, store.Prefix+"machines/?op=allocate&tags=worker", http.StatusConflict)

	deployed, err := client.Machine.Deploy(machine.SystemID, &entity.MachineDeployParams{
		UserData: base64.StdEncoding.EncodeToString([]byte("#cloud-config\n")), DistroSeries: "jammy",
	})
	if err != nil || deployed.Status != node.StatusDeploying {
		t.Fatalf("deploy machine: %+v, %v", deployed, err)
	}
	deployed = waitForStatus(t, client, machine.SystemID, node.StatusDeployed)
	if deployed.DistroSeries != "jammy" || len(deployed.IPAddresses) != 1 || len(deployed.InterfaceSet) != 1 {
		t.Fatalf("deployed machine lacks IP, interface or distro: %+v", deployed)
	}
	nodeIP := deployed.IPAddresses[0].String()
	if linkIP := deployed.InterfaceSet[0].Links[0].IPAddress; linkIP != nodeIP {
		t.Fatalf("interface IP = %q, want %q", linkIP, nodeIP)
	}
	expectStatus(t, server, http.MethodPost, operation+"op-power_off", http.StatusOK)
	power, err := client.Machine.GetPowerState(machine.SystemID)
	if err != nil || power.State != "off" {
		t.Fatalf("get power state: %+v, %v", power, err)
	}
	if _, err := client.Machine.PowerOn(machine.SystemID, &entity.MachinePowerOnParams{}); err != nil {
		t.Fatal(err)
	}

	reservation, err := client.IPAddresses.Reserve(&entity.IPAddressesParams{})
	if err != nil || reservation.IP.String() == nodeIP {
		t.Fatalf("reserve IP: %+v, %v", reservation, err)
	}
	if err := client.IPAddresses.Release(&entity.IPAddressesParams{IP: reservation.IP.String()}); err != nil {
		t.Fatal(err)
	}

	record, err := client.DNSResources.Create(&entity.DNSResourceParams{FQDN: "cluster.lab.test", AddressTTL: 10})
	if err != nil {
		t.Fatal(err)
	}
	record, err = client.DNSResource.Update(record.ID, &entity.DNSResourceParams{IPAddresses: nodeIP, AddressTTL: 10})
	if err != nil || len(record.IPAddresses) != 1 || record.IPAddresses[0].IP.String() != nodeIP {
		t.Fatalf("update DNS resource: %+v, %v", record, err)
	}
	if got := resolveDNS(t, handler, record.FQDN); got != nodeIP {
		t.Fatalf("DNS answer = %q, want recorded address %q", got, nodeIP)
	}
	records, err := client.DNSResources.Get(&entity.DNSResourcesParams{FQDN: record.FQDN})
	if err != nil || len(records) != 1 || records[0].ID != record.ID {
		t.Fatalf("find DNS resource: %+v, %v", records, err)
	}

	released, err := client.Machine.Release(machine.SystemID, &entity.MachineReleaseParams{})
	if err != nil || released.Status != node.StatusReady || len(released.IPAddresses) != 0 {
		t.Fatalf("release machine: %+v, %v", released, err)
	}
	if len(released.TagNames) != 1 || released.TagNames[0] != "worker" {
		t.Fatalf("release left unexpected tags: %v", released.TagNames)
	}
	record, err = client.DNSResource.Get(record.ID)
	if err != nil || len(record.IPAddresses) != 0 {
		t.Fatalf("get detached DNS resource: %+v, %v", record, err)
	}
	if err := client.DNSResource.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAuthAndMultipartDeploymentCancellation(t *testing.T) {
	server, _, client := start(t, driver.NewMemory(time.Second), seed.Default(1))
	path := store.Prefix + "version/"
	for _, header := range []string{"", "Basic abc", `OAuth oauth_token="def"`, "OAuth oauth_token=def"} {
		req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", header)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("malformed authorization returned %d", res.StatusCode)
		}
	}

	machine, err := client.Machines.Allocate(&entity.MachineAllocateParams{})
	if err != nil {
		t.Fatal(err)
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	values := map[string]string{"op": "deploy", "user_data": base64.StdEncoding.EncodeToString([]byte("#cloud-config"))}
	for name, value := range values {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path = machine.ResourceURI
	req, err := http.NewRequest(http.MethodPost, server.URL+path, &form)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var reply entity.Machine
	if err := json.NewDecoder(res.Body).Decode(&reply); err != nil || reply.Status != node.StatusDeploying {
		t.Fatalf("multipart deploy: status %d, machine %+v, error %v", res.StatusCode, reply, err)
	}
	released, err := client.Machine.Release(machine.SystemID, &entity.MachineReleaseParams{})
	if err != nil || released.Status != node.StatusReady {
		t.Fatalf("release ongoing deploy: %+v, %v", released, err)
	}
	ip, err := client.IPAddresses.Get(&entity.IPAddressesParams{})
	if err != nil || len(ip) != 0 {
		t.Fatalf("cancelled deployment left IPs: %+v, %v", ip, err)
	}
}

func TestConcurrentAllocation(t *testing.T) {
	server, _, _ := start(t, driver.NewMemory(0), seed.Default(4))
	results := make(chan string, 12)
	var requests sync.WaitGroup
	for range cap(results) {
		requests.Add(1)
		go func() {
			defer requests.Done()
			client, err := maas.GetClient(server.URL+"/MAAS", "abc:def:ghi", "2.0")
			if err != nil {
				t.Error(err)
				return
			}
			machine, err := client.Machines.Allocate(&entity.MachineAllocateParams{})
			if err == nil {
				results <- machine.SystemID
			} else if !strings.Contains(err.Error(), "409") {
				t.Error(err)
			}
		}()
	}
	requests.Wait()
	close(results)
	ids := map[string]bool{}
	for id := range results {
		if ids[id] {
			t.Fatalf("allocated %s more than once", id)
		}
		ids[id] = true
	}
	if len(ids) != 4 {
		t.Fatalf("allocated %d distinct machines, want 4", len(ids))
	}
}

func TestDockerNodeLifecycle(t *testing.T) {
	if os.Getenv("MAAS_SIM_TEST_DOCKER") != "1" {
		t.Skip("set MAAS_SIM_TEST_DOCKER=1 to use a local Docker daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := os.Getenv("MAAS_SIM_TEST_NODE_IMAGE")
	if image == "" {
		image = "maas-sim-test-node:local"
	}
	backend, err := driver.NewDocker(ctx, driver.DockerOptions{
		NetworkName: "maas-sim-test-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Image:       image,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	_, handler, client := start(t, backend, seed.Default(1))

	reservation, err := client.IPAddresses.Reserve(&entity.IPAddressesParams{})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := client.Machines.Allocate(&entity.MachineAllocateParams{})
	if err != nil {
		t.Fatal(err)
	}
	userData := `#cloud-config
write_files:
  - path: /srv/seed-test/startup.txt
    content: hello from cloud-init
`
	_, err = client.Machine.Deploy(machine.SystemID, &entity.MachineDeployParams{
		UserData: base64.StdEncoding.EncodeToString([]byte(userData)),
	})
	if err != nil {
		t.Fatal(err)
	}
	machine = waitForStatus(t, client, machine.SystemID, node.StatusDeployed)
	nodeIP, err := netip.ParseAddr(machine.IPAddresses[0].String())
	if err != nil || !backend.Network().Subnet.Contains(nodeIP) || nodeIP.String() == reservation.IP.String() {
		t.Fatalf("node address %q is invalid or collides with reservation %s", nodeIP, reservation.IP)
	}
	record, err := client.DNSResources.Create(&entity.DNSResourceParams{
		FQDN: "docker-test.maas", IPAddresses: nodeIP.String(), AddressTTL: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveDNS(t, handler, record.FQDN); got != nodeIP.String() {
		t.Fatalf("DNS answer = %q, want %q", got, nodeIP)
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(nodeIP.String(), "8080"), Path: "/startup.txt"}
	httpClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{}}
	defer httpClient.CloseIdleConnections()
	deadline := time.Now().Add(time.Minute)
	for {
		response, err := httpClient.Get(endpoint.String())
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && string(body) == "hello from cloud-init" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cloud-init did not serve the seeded file at %s: %v", endpoint.String(), err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := client.Machine.Release(machine.SystemID, &entity.MachineReleaseParams{}); err != nil {
		t.Fatal(err)
	}
	if err := client.IPAddresses.Release(&entity.IPAddressesParams{IP: reservation.IP.String()}); err != nil {
		t.Fatal(err)
	}
	if err := client.DNSResource.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
}

func waitForStatus(t *testing.T, client *maas.Client, id string, expected node.Status) *entity.Machine {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		machine, err := client.Machine.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if machine.Status == expected {
			return machine
		}
		if machine.Status == node.StatusFailedDeployment || time.Now().After(deadline) {
			t.Fatalf("machine %s is %s, want status %d", id, machine.StatusName, expected)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func expectStatus(t *testing.T, server *httptest.Server, method, path string, expected int) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", authHeader)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != expected {
		t.Fatalf("%s %s returned %d, want %d", method, path, res.StatusCode, expected)
	}
}

func resolveDNS(t *testing.T, handler *api.Server, name string) string {
	t.Helper()
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	server := &dns.Server{
		PacketConn: listener, Handler: handler.DNSHandler(), NotifyStartedFunc: func() { close(ready) },
	}
	go func() { _ = server.ActivateAndServe() }()
	<-ready
	defer server.Shutdown()
	message := new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA)
	response, _, err := new(dns.Client).Exchange(message, listener.LocalAddr().String())
	if err != nil || len(response.Answer) != 1 {
		t.Fatalf("resolve %s: %+v, %v", name, response, err)
	}
	answer, ok := response.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("expected A answer, got %+v", response.Answer)
	}
	return answer.A.String()
}
