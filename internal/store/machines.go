package store

import (
	"fmt"
	"net"
	"slices"

	"github.com/canonical/gomaasclient/entity"
	"github.com/canonical/gomaasclient/entity/node"

	"github.com/guilycst/maas-sim/internal/dnsname"
	"github.com/guilycst/maas-sim/internal/driver"
)

type MachineFilter struct {
	Hostnames []string
	IDs       []string
	Tags      []string
	NotTags   []string
	Zones     []string
	Pools     []string
	Statuses  []node.Status
	CPU       int64
	Memory    int64
}

func (f MachineFilter) matches(m entity.Machine) bool {
	if !oneOf(f.Hostnames, m.Hostname) && !oneOf(f.Hostnames, m.FQDN) {
		return false
	}
	if !oneOf(f.IDs, m.SystemID) || !oneOf(f.Zones, m.Zone.Name) || !oneOf(f.Pools, m.Pool.Name) {
		return false
	}
	if !oneOf(f.Statuses, m.Status) || int64(m.CPUCount) < f.CPU || m.Memory < f.Memory {
		return false
	}
	for _, tag := range f.Tags {
		if !slices.Contains(m.TagNames, tag) {
			return false
		}
	}
	for _, tag := range f.NotTags {
		if slices.Contains(m.TagNames, tag) {
			return false
		}
	}
	return true
}

func oneOf[T comparable](values []T, actual T) bool {
	return len(values) == 0 || slices.Contains(values, actual)
}

func (s *Store) Machines(filter MachineFilter) []entity.Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	machines := make([]entity.Machine, 0, len(s.machines))
	for _, machine := range s.machines {
		if filter.matches(machine) {
			machines = append(machines, machine)
		}
	}
	return machines
}

func (s *Store) Machine(id string) (entity.Machine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	i, ok := s.machineIDs[id]
	if !ok {
		return entity.Machine{}, false
	}
	return s.machines[i], true
}

func (s *Store) Allocate(filter MachineFilter, dryRun bool) (entity.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, machine := range s.machines {
		if machine.Status != node.StatusReady || !filter.matches(machine) {
			continue
		}
		if !dryRun {
			SetStatus(&machine, node.StatusAllocated, "Allocated")
			machine.Owner = Admin.UserName
			s.machines[i] = machine
		}
		return machine, nil
	}
	return entity.Machine{}, fmt.Errorf("%w: no matching machines available", ErrConflict)
}

func (s *Store) ChangeMachine(id string, change func(*entity.Machine) error) (entity.Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.machineIDs[id]
	if !ok {
		return entity.Machine{}, ErrNotFound
	}
	machine := s.machines[i]
	if err := change(&machine); err != nil {
		return entity.Machine{}, err
	}
	if machine.Hostname != s.machines[i].Hostname {
		if !dnsname.Valid(machine.FQDN) {
			return entity.Machine{}, fmt.Errorf("%w: invalid machine FQDN", ErrInvalid)
		}
		for _, existing := range s.machines {
			if existing.Hostname == machine.Hostname {
				return entity.Machine{}, fmt.Errorf("%w: hostname %q already exists", ErrConflict, machine.Hostname)
			}
		}
	}
	s.machines[i] = machine
	return machine, nil
}

func SetStatus(machine *entity.Machine, status node.Status, name string) {
	machine.Status, machine.StatusName, machine.StatusMessage = status, name, name
}

func (s *Store) SetInstance(machine *entity.Machine, instance driver.Instance) {
	nic := machine.BootInterface
	link := entity.NetworkInterfaceLink{ID: nic.ID, Mode: "auto", Subnet: s.topology.Subnet}
	machine.IPAddresses = nil
	if instance.IP.IsValid() {
		link.IPAddress = instance.IP.String()
		machine.IPAddresses = []net.IP{net.IP(instance.IP.AsSlice())}
	}
	if instance.MAC != "" {
		nic.MACAddress = instance.MAC
	}
	nic.Links = []entity.NetworkInterfaceLink{link}
	machine.BootInterface = nic
	machine.InterfaceSet = []entity.NetworkInterface{nic}
}

func (s *Store) ResetMachine(machine *entity.Machine) {
	SetStatus(machine, node.StatusReady, "Ready")
	machine.PowerState, machine.Owner = "off", ""
	machine.DistroSeries, machine.OSystem, machine.HWEKernel = "", "", ""
	machine.EphemeralDeploy = false
	machine.TagNames = s.seedTags[machine.SystemID]
	s.SetInstance(machine, driver.Instance{})
}
