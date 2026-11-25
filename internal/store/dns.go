package store

import (
	"net/netip"
	"slices"
	"strings"

	"github.com/canonical/gomaasclient/entity"
)

func (s *Store) DNSRecords() []entity.DNSResource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]entity.DNSResource, 0, len(s.dns))
	for _, record := range s.dns {
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b entity.DNSResource) int { return a.ID - b.ID })
	return records
}

func (s *Store) DNSRecord(id int) (entity.DNSResource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.dns[id]
	return record, ok
}

func (s *Store) SaveDNS(record entity.DNSResource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.ID >= s.nextDNSID {
		s.nextDNSID = record.ID + 1
	}
	s.dns[record.ID] = record
}

func (s *Store) NextDNSID() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextDNSID
}

func (s *Store) RemoveDNS(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dns, id)
}

func (s *Store) Lookup(name string) (ips []netip.Addr, ttl int, exists bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, record := range s.dns {
		if record.FQDN != name {
			continue
		}
		ttl = record.AddressTTL
		for _, address := range record.IPAddresses {
			if ip, ok := netip.AddrFromSlice(address.IP); ok {
				ips = append(ips, ip.Unmap())
			}
		}
		return ips, ttl, true
	}
	for ip, reservation := range s.reservedIPs {
		if reservation.Hostname == name {
			return []netip.Addr{ip}, s.domain.TTL, true
		}
	}
	for _, machine := range s.machines {
		if machine.FQDN != name {
			continue
		}
		for _, address := range machine.IPAddresses {
			if ip, ok := netip.AddrFromSlice(address); ok {
				ips = append(ips, ip.Unmap())
			}
		}
		return ips, s.domain.TTL, true
	}
	return nil, 0, false
}

type IPReservation struct {
	Address  entity.IPAddress
	Hostname string
	LeaseID  string
}

func (s *Store) ReservedIP(ip netip.Addr) (IPReservation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.reservedIPs[ip]
	return entry, ok
}

func (s *Store) SaveIP(ip netip.Addr, entry IPReservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservedIPs[ip] = entry
}

func (s *Store) ReleaseIP(ip netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reservedIPs, ip)
}

func (s *Store) IPAddresses() []entity.IPAddress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	addresses := make([]entity.IPAddress, 0, len(s.reservedIPs))
	for _, entry := range s.reservedIPs {
		addresses = append(addresses, entry.Address)
	}
	for _, machine := range s.machines {
		for _, ip := range machine.IPAddresses {
			address, _ := netip.AddrFromSlice(ip)
			if _, reserved := s.reservedIPs[address.Unmap()]; reserved {
				continue
			}
			entry := s.IPAddress(address.Unmap(), "Auto")
			entry.InterfaceSet = machine.InterfaceSet
			addresses = append(addresses, entry)
		}
	}
	slices.SortFunc(addresses, func(a, b entity.IPAddress) int {
		return strings.Compare(a.IP.String(), b.IP.String())
	})
	return addresses
}
