package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"github.com/canonical/gomaasclient/entity"
	"github.com/canonical/gomaasclient/entity/node"

	"github.com/guilycst/maas-sim/internal/dnsname"
	"github.com/guilycst/maas-sim/internal/driver"
	"github.com/guilycst/maas-sim/internal/seed"
)

const Prefix = "/MAAS/api/2.0/"

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
	ErrInvalid  = errors.New("invalid resource")
	Admin       = entity.User{
		UserName: "admin", Email: "admin@maas.invalid",
		ResourceURI: Prefix + "users/admin/", IsSuperUser: true, IsLocal: true,
	}
)

type Store struct {
	mu          sync.RWMutex
	machines    []entity.Machine
	machineIDs  map[string]int
	seedTags    map[string][]string
	tags        map[string]entity.Tag
	domain      entity.Domain
	topology    Topology
	dns         map[int]entity.DNSResource
	nextDNSID   int
	reservedIPs map[netip.Addr]IPReservation
}

type Topology struct {
	Subnet  entity.Subnet
	Range   entity.IPRange
	Fabric  entity.Fabric
	VLAN    entity.VLAN
	Zones   []entity.Zone
	Pools   []entity.ResourcePool
	Network driver.Network
}

func New(inventory seed.Inventory, network driver.Network) *Store {
	machineCount := len(inventory.Machines)
	zones := make([]entity.Zone, 0, len(inventory.Zones))
	zoneByName := make(map[string]entity.Zone, len(inventory.Zones))
	for i, name := range inventory.Zones {
		zone := entity.Zone{ID: i + 1, Name: name, ResourceURI: Prefix + "zones/" + name + "/"}
		zones = append(zones, zone)
		zoneByName[name] = zone
	}
	pools := make([]entity.ResourcePool, 0, len(inventory.ResourcePools))
	poolByName := make(map[string]entity.ResourcePool, len(inventory.ResourcePools))
	for i, name := range inventory.ResourcePools {
		id := i + 1
		pool := entity.ResourcePool{ID: id, Name: name, ResourceURI: fmt.Sprintf("%sresourcepools/%d/", Prefix, id)}
		pools = append(pools, pool)
		poolByName[name] = pool
	}
	vlan := entity.VLAN{
		ID: 1, Name: "untagged", Fabric: "fabric-0", FabricID: 1,
		MTU: 1500, ResourceURI: Prefix + "fabrics/1/vlans/0/",
	}
	subnet := entity.Subnet{
		ID: 1, Name: "simulator", CIDR: network.Subnet.String(), GatewayIP: net.IP(network.Gateway.AsSlice()),
		VLAN: vlan, Managed: true, AllowDNS: true, ResourceURI: Prefix + "subnets/1/",
	}
	s := &Store{
		machines: make([]entity.Machine, 0, machineCount), machineIDs: make(map[string]int, machineCount),
		seedTags: make(map[string][]string, machineCount), tags: make(map[string]entity.Tag),
		domain: entity.Domain{
			ID: 1, Name: inventory.Domain, TTL: 10, Authoritative: true, IsDefault: true,
			ResourceURI: Prefix + "domains/1/",
		},
		topology: Topology{
			Subnet: subnet, VLAN: vlan, Network: network,
			Fabric: entity.Fabric{
				ID: 1, Name: vlan.Fabric, VLANs: []entity.VLAN{vlan}, ResourceURI: Prefix + "fabrics/1/",
			},
			Range: entity.IPRange{
				ID: 1, Type: "dynamic", Subnet: subnet, ResourceURI: Prefix + "ipranges/1/",
				StartIP: net.IP(network.Gateway.Next().AsSlice()), EndIP: net.IP(broadcast(network.Subnet).Prev().AsSlice()),
			},
			Zones: zones, Pools: pools,
		},
		dns: make(map[int]entity.DNSResource), nextDNSID: 1, reservedIPs: make(map[netip.Addr]IPReservation),
	}
	for _, name := range inventory.Tags {
		s.tags[name] = NewTag(name)
	}
	for i, initial := range inventory.Machines {
		id, hostname, tags := initial.SystemID, initial.Hostname, slices.Clone(initial.Tags)
		mac := sha256.Sum256([]byte(id))
		nic := entity.NetworkInterface{
			ID: i + 1, Name: "eth0", SystemID: id, Type: "physical", Enabled: true, LinkConnected: true,
			MACAddress:  fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4]),
			VLAN:        vlan,
			ResourceURI: fmt.Sprintf("%smachines/%s/interfaces/%d/", Prefix, id, i+1),
			Links:       []entity.NetworkInterfaceLink{{ID: i + 1, Mode: "auto", Subnet: subnet}},
		}
		storageMB := float64(initial.DiskGiB) * (1 << 30) / 1e6
		if initial.DiskGiB == 0 {
			storageMB = 100000 // Built-in inventory predates the versioned seed.
		}
		machine := entity.Machine{
			SystemID: id, Hostname: hostname, Domain: s.domain, FQDN: hostname + "." + s.domain.Name,
			CPUCount: initial.CPUCount, Memory: initial.MemoryMiB, Architecture: "amd64/generic", Storage: storageMB,
			Zone: zoneByName[initial.Zone], Pool: poolByName[initial.ResourcePool],
			PowerState: "off", PowerType: "manual", NodeTypeName: "Machine",
			TagNames: tags, BootInterface: nic, InterfaceSet: []entity.NetworkInterface{nic},
			ResourceURI: Prefix + "machines/" + id + "/",
		}
		SetStatus(&machine, node.StatusReady, "Ready")
		s.machineIDs[id] = i
		s.seedTags[id] = tags
		s.machines = append(s.machines, machine)
	}
	return s
}

func (s *Store) Topology() Topology { return s.topology }

func (s *Store) Domain() entity.Domain {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.domain
}

func (s *Store) SetDomain(change func(*entity.Domain)) (entity.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	domain := s.domain
	change(&domain)
	for _, machine := range s.machines {
		if !dnsname.Valid(machine.Hostname + "." + domain.Name) {
			return entity.Domain{}, fmt.Errorf("%w: domain would make machine %q FQDN invalid", ErrInvalid, machine.Hostname)
		}
	}
	s.domain = domain
	for i := range s.machines {
		s.machines[i].Domain = s.domain
		s.machines[i].FQDN = s.machines[i].Hostname + "." + s.domain.Name
	}
	return s.domain, nil
}

func (s *Store) ValidIP(ip netip.Addr) bool {
	n := s.topology.Network
	return n.Subnet.Contains(ip) && ip != n.Subnet.Addr() && ip != n.Gateway && ip != broadcast(n.Subnet)
}

func (s *Store) IPAddress(ip netip.Addr, kind string) entity.IPAddress {
	address := entity.IPAddress{IP: net.IP(ip.AsSlice()), Subnet: s.topology.Subnet, AllocTypeName: kind}
	if kind == "User reserved" {
		address.Owner, address.AllocType = Admin, 4
	}
	return address
}

func broadcast(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Masked().Addr().As4()
	for i := prefix.Bits(); i < 32; i++ {
		bytes[i/8] |= 1 << (7 - uint(i%8))
	}
	return netip.AddrFrom4(bytes)
}

func NewTag(name string) entity.Tag {
	return entity.Tag{Name: name, ResourceURI: Prefix + "tags/" + name + "/"}
}

func (s *Store) Tags() []entity.Tag {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tags := make([]entity.Tag, 0, len(s.tags))
	for _, tag := range s.tags {
		tags = append(tags, tag)
	}
	slices.SortFunc(tags, func(a, b entity.Tag) int { return strings.Compare(a.Name, b.Name) })
	return tags
}

func (s *Store) Tag(name string) (entity.Tag, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tag, ok := s.tags[name]
	return tag, ok
}

func (s *Store) AddTag(tag entity.Tag) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tags[tag.Name]; ok {
		return ErrConflict
	}
	s.tags[tag.Name] = tag
	return nil
}

func (s *Store) UpdateTagNodes(name string, add, remove []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tags[name]; !ok {
		return ErrNotFound
	}
	for _, id := range slices.Concat(add, remove) {
		if _, ok := s.machineIDs[id]; !ok {
			return fmt.Errorf("%w: machine %s", ErrNotFound, id)
		}
	}
	for _, id := range add {
		machine := &s.machines[s.machineIDs[id]]
		if !slices.Contains(machine.TagNames, name) {
			machine.TagNames = append(slices.Clone(machine.TagNames), name)
		}
	}
	for _, id := range remove {
		machine := &s.machines[s.machineIDs[id]]
		machine.TagNames = slices.DeleteFunc(slices.Clone(machine.TagNames), func(tag string) bool { return tag == name })
	}
	return nil
}
