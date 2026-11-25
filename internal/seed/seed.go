package seed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/guilycst/maas-sim/internal/dnsname"
)

const (
	schemaVersion = 1
	maxMachines   = 10000
	defaultGroup  = "default"
	maxSeedBytes  = 8 << 20
)

var (
	identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	maasName   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
)

type Inventory struct {
	SchemaVersion int       `yaml:"schemaVersion"`
	Domain        string    `yaml:"domain"`
	Zones         []string  `yaml:"zones"`
	ResourcePools []string  `yaml:"resourcePools"`
	Tags          []string  `yaml:"tags"`
	Machines      []Machine `yaml:"machines"`
}

type Machine struct {
	SystemID     string   `yaml:"systemID"`
	Hostname     string   `yaml:"hostname"`
	Zone         string   `yaml:"zone"`
	ResourcePool string   `yaml:"resourcePool"`
	Tags         []string `yaml:"tags"`
	CPUCount     int      `yaml:"cpuCount"`
	MemoryMiB    int64    `yaml:"memoryMiB"`
	DiskGiB      int64    `yaml:"diskGiB"`
}

func Default(count int) Inventory {
	inventory := Inventory{
		SchemaVersion: schemaVersion, Domain: "maas",
		Zones: []string{"default", "az1", "az2"}, ResourcePools: []string{"default"},
		Tags: []string{"control-plane", "worker"}, Machines: make([]Machine, 0, count),
	}
	for i := range count {
		inventory.Machines = append(inventory.Machines, Machine{
			SystemID: fmt.Sprintf("sim%03d", i+1), Hostname: fmt.Sprintf("node-%03d", i+1),
			Zone: inventory.Zones[i%len(inventory.Zones)], ResourcePool: defaultGroup,
			Tags: []string{inventory.Tags[i%len(inventory.Tags)]}, CPUCount: 8, MemoryMiB: 16384,
		})
	}
	return inventory
}

func Load(path string) (Inventory, error) {
	inventory, err := load(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("seed %q: %w", path, err)
	}
	return inventory, nil
}

func load(path string) (Inventory, error) {
	file, err := os.Open(path)
	if err != nil {
		return Inventory{}, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxSeedBytes+1))
	if err != nil {
		return Inventory{}, err
	}
	if len(data) > maxSeedBytes {
		return Inventory{}, errors.New("seed exceeds 8 MiB")
	}
	if strings.EqualFold(filepath.Ext(path), ".json") && !json.Valid(data) {
		return Inventory{}, errors.New("invalid JSON")
	}

	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Inventory{}, err
	}
	if err := validateDocument(&raw, ""); err != nil {
		return Inventory{}, err
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var inventory Inventory
	if err := decoder.Decode(&inventory); err != nil {
		return Inventory{}, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Inventory{}, errors.New("exactly one document is required")
	}
	if err := inventory.validate(); err != nil {
		return Inventory{}, err
	}
	return inventory, nil
}

func validateDocument(node *yaml.Node, path string) error {
	if node.Kind == yaml.AliasNode || node.Tag == "!!merge" {
		return fmt.Errorf("%s at line %d: yaml aliases and merges are not supported", path, node.Line)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag != "!!str" {
				return fmt.Errorf("%s at line %d: expected a string field name", path, key.Line)
			}
			field := key.Value
			if path != "" {
				field = path + "." + field
			}
			if seen[key.Value] {
				return fmt.Errorf("%s at line %d: duplicate field", field, key.Line)
			}
			seen[key.Value] = true
			switch key.Value {
			case "systemID", "hostname", "domain", "zone", "resourcePool":
				if value.Tag != "!!str" || value.Value == "" {
					return fmt.Errorf("%s: expected a nonempty string", field)
				}
			case "schemaVersion", "cpuCount", "memoryMiB", "diskGiB":
				if value.Tag != "!!int" {
					return fmt.Errorf("%s: expected an integer", field)
				}
			case "zones", "resourcePools", "tags":
				if value.Kind != yaml.SequenceNode {
					return fmt.Errorf("%s: expected an array of strings", field)
				}
				for i, item := range value.Content {
					if item.Tag != "!!str" {
						return fmt.Errorf("%s[%d]: expected a string", field, i)
					}
				}
			}
			if err := validateDocument(value, field); err != nil {
				return err
			}
		}
		return nil
	}
	if node.Kind == yaml.SequenceNode || node.Kind == yaml.DocumentNode {
		for i, child := range node.Content {
			childPath := path
			if node.Kind == yaml.SequenceNode {
				childPath = fmt.Sprintf("%s[%d]", path, i)
			}
			if err := validateDocument(child, childPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Inventory) validate() error {
	if s.SchemaVersion != schemaVersion {
		return fmt.Errorf("schemaVersion must be %d", schemaVersion)
	}
	if s.Machines == nil || len(s.Machines) > maxMachines {
		return fmt.Errorf("machines must be an array with at most %d entries", maxMachines)
	}
	if s.Domain == "" {
		s.Domain = "maas"
	}
	s.Domain = strings.ToLower(s.Domain)
	if !dnsname.Valid(s.Domain) {
		return errors.New("domain must be a valid DNS name")
	}
	if s.Zones == nil {
		s.Zones = []string{defaultGroup}
	}
	if s.ResourcePools == nil {
		s.ResourcePools = []string{defaultGroup}
	}
	zones, err := names(s.Zones, "zones")
	if err != nil {
		return err
	}
	pools, err := names(s.ResourcePools, "resourcePools")
	if err != nil {
		return err
	}
	tags, err := names(s.Tags, "tags")
	if err != nil {
		return err
	}

	hosts, ids := map[string]int{}, map[string]int{}
	for index := range s.Machines {
		machine := &s.Machines[index]
		if err := machine.normalize(zones, pools, tags); err != nil {
			return fmt.Errorf("machines[%d].%w", index, err)
		}
		if !dnsname.Valid(machine.Hostname + "." + s.Domain) {
			return fmt.Errorf("machines[%d].hostname: combined hostname and domain must form a valid DNS name", index)
		}
		if other, exists := hosts[machine.Hostname]; exists {
			return fmt.Errorf("machines[%d].hostname duplicates machines[%d].hostname", index, other)
		}
		if other, exists := ids[machine.SystemID]; exists {
			return fmt.Errorf("machines[%d].systemID duplicates machines[%d].systemID (%q)", index, other, machine.SystemID)
		}
		hosts[machine.Hostname], ids[machine.SystemID] = index, index
	}
	return nil
}

// ValidResourceName reports whether name can appear in a MAAS inventory path.
func ValidResourceName(name string) bool { return maasName.MatchString(name) }

func names(values []string, field string) (map[string]bool, error) {
	index := make(map[string]bool, len(values))
	for i, value := range values {
		if !ValidResourceName(value) {
			return nil, fmt.Errorf("%s[%d]: invalid name %q", field, i, value)
		}
		if index[value] {
			return nil, fmt.Errorf("%s[%d]: duplicate name %q", field, i, value)
		}
		index[value] = true
	}
	return index, nil
}

func (m *Machine) normalize(zones, pools, tags map[string]bool) error {
	m.Hostname = strings.ToLower(m.Hostname)
	if !dnsname.ValidLabel(m.Hostname) {
		return errors.New("hostname: expected a short DNS name")
	}
	if m.SystemID == "" {
		sum := sha256.Sum256([]byte(m.Hostname))
		m.SystemID = hex.EncodeToString(sum[:6])
	}
	if !identifier.MatchString(m.SystemID) {
		return errors.New("systemID: expected a lowercase alphanumeric ID, optionally containing hyphens")
	}
	if m.Zone == "" {
		m.Zone = defaultGroup
	}
	if !zones[m.Zone] {
		return fmt.Errorf("zone: unknown zone %q", m.Zone)
	}
	if m.ResourcePool == "" {
		m.ResourcePool = defaultGroup
	}
	if !pools[m.ResourcePool] {
		return fmt.Errorf("resourcePool: unknown resource pool %q", m.ResourcePool)
	}
	_, err := names(m.Tags, "tags")
	if err != nil {
		return err
	}
	for i, tag := range m.Tags {
		if !tags[tag] {
			return fmt.Errorf("tags[%d]: unknown tag %q", i, tag)
		}
	}
	for _, value := range []struct {
		name  string
		count int64
	}{
		{name: "cpuCount", count: int64(m.CPUCount)},
		{name: "memoryMiB", count: m.MemoryMiB},
		{name: "diskGiB", count: m.DiskGiB},
	} {
		if value.count <= 0 {
			return fmt.Errorf("%s: expected a positive integer", value.name)
		}
	}
	return nil
}
