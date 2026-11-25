package api

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/canonical/gomaasclient/entity"
	"github.com/miekg/dns"

	"github.com/guilycst/maas-sim/internal/dnsname"
	"github.com/guilycst/maas-sim/internal/store"
)

const maxTTL = 2147483647

func (s *Server) dnsRoutes() {
	s.route(http.MethodGet, "dnsresources/{$}", s.listDNS)
	s.route(http.MethodPost, "dnsresources/{$}", s.writeDNS)
	s.route(http.MethodDelete, "dnsresources/{$}", s.deleteDNS)
	s.route(http.MethodGet, "dnsresources/{id}/{$}", s.getDNS)
	s.route(http.MethodPut, "dnsresources/{id}/{$}", s.writeDNS)
	s.route(http.MethodPost, "dnsresources/{id}/{$}", s.writeDNS)
	s.route(http.MethodDelete, "dnsresources/{id}/{$}", s.deleteDNS)
}

func (s *Server) listDNS(w http.ResponseWriter, r *http.Request) {
	records := []entity.DNSResource{}
	fqdn, name, domain := getParam(r, "fqdn"), getParam(r, "name"), getParam(r, "domain")
	for _, record := range s.store.DNSRecords() {
		if fqdn != "" && record.FQDN != strings.TrimSuffix(strings.ToLower(fqdn), ".") {
			continue
		}
		if name != "" && !strings.HasPrefix(record.FQDN, name+".") {
			continue
		}
		domainName := s.store.Domain().Name
		if domain != "" && domain != strconv.Itoa(s.store.Domain().ID) {
			domainName = domain
		}
		if domain != "" && !strings.HasSuffix(record.FQDN, "."+domainName) {
			continue
		}
		records = append(records, record)
	}
	writeJSON(w, http.StatusOK, records)
}

func (s *Server) getDNS(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	record, ok := s.store.DNSRecord(id)
	if !ok {
		fail(w, http.StatusNotFound, "DNS resource not found")
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) writeDNS(w http.ResponseWriter, r *http.Request) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()

	idText := r.PathValue("id")
	id, _ := strconv.Atoi(idText)
	current, exists := s.store.DNSRecord(id)
	if idText != "" && !exists {
		fail(w, http.StatusNotFound, "DNS resource not found")
		return
	}
	fqdn := getParam(r, "fqdn")
	if name := getParam(r, "name"); fqdn == "" && name != "" {
		domain := getParam(r, "domain")
		if domain == "" || domain == strconv.Itoa(s.store.Domain().ID) {
			domain = s.store.Domain().Name
		}
		fqdn = name + "." + domain
	}
	if fqdn == "" {
		fqdn = current.FQDN
	}
	fqdn = strings.TrimSuffix(strings.ToLower(fqdn), ".")
	if !dnsname.Valid(fqdn) {
		fail(w, http.StatusBadRequest, "valid fqdn or name required")
		return
	}
	for _, record := range s.store.DNSRecords() {
		if record.FQDN == fqdn && record.ID != id {
			fail(w, http.StatusConflict, "DNS resource already exists")
			return
		}
	}
	ttl, err := minimum(r, "address_ttl")
	if err != nil || ttl > maxTTL {
		fail(w, http.StatusBadRequest, "invalid address ttl")
		return
	}
	updated := current
	if !exists {
		id = s.store.NextDNSID()
		updated = entity.DNSResource{
			ID: id, AddressTTL: s.store.Domain().TTL, ResourceURI: fmt.Sprintf("%sdnsresources/%d/", store.Prefix, id),
		}
	}
	updated.FQDN = fqdn
	if _, hasTTL := formValues(r, "address_ttl"); hasTTL && ttl > 0 {
		updated.AddressTTL = int(ttl)
	}
	if _, supplied := formValues(r, "ip_addresses"); supplied {
		ips, err := parseIPs(listParams(r, "ip_addresses"))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		updated.IPAddresses = make([]entity.IPAddress, 0, len(ips))
		for _, address := range ips {
			updated.IPAddresses = append(updated.IPAddresses, s.store.IPAddress(address, "Auto"))
		}
	}

	s.store.SaveDNS(updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteDNS(w http.ResponseWriter, r *http.Request) {
	s.networkMu.Lock()
	defer s.networkMu.Unlock()

	idText := r.PathValue("id")
	if idText == "" {
		idText = getParam(r, "id")
	}
	id, _ := strconv.Atoi(idText)
	record, exists := s.store.DNSRecord(id)
	if idText == "" {
		fqdn := getParam(r, "fqdn")
		for _, candidate := range s.store.DNSRecords() {
			if candidate.FQDN == fqdn {
				record, exists = candidate, true
				break
			}
		}
	}
	if !exists {
		fail(w, http.StatusNotFound, "DNS resource not found")
		return
	}
	s.store.RemoveDNS(record.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) detachAddress(ip netip.Addr) {
	for _, record := range s.store.DNSRecords() {
		addresses := slices.DeleteFunc(slices.Clone(record.IPAddresses), func(address entity.IPAddress) bool {
			return address.IP.Equal(net.IP(ip.AsSlice()))
		})
		if len(addresses) != len(record.IPAddresses) {
			record.IPAddresses = addresses
			s.store.SaveDNS(record)
		}
	}
}

func parseIPs(values []string) ([]netip.Addr, error) {
	var addresses []netip.Addr
	for _, value := range values {
		for _, address := range strings.Fields(value) {
			ip, err := netip.ParseAddr(address)
			if err != nil || !ip.Is4() {
				return nil, fmt.Errorf("invalid IPv4 address %q", address)
			}
			if !slices.Contains(addresses, ip) {
				addresses = append(addresses, ip)
			}
		}
	}
	slices.SortFunc(addresses, netip.Addr.Compare)
	return addresses, nil
}

func (s *Server) DNSHandler() dns.Handler {
	return dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(r)
		response.Authoritative = true
		if len(r.Question) != 1 {
			response.Rcode = dns.RcodeFormatError
		} else {
			q := r.Question[0]
			addresses, ttl, exists := s.store.Lookup(q.Name)
			if !exists {
				response.Rcode = dns.RcodeNameError
			} else if q.Qtype == dns.TypeA && q.Qclass == dns.ClassINET {
				for _, ip := range addresses {
					response.Answer = append(response.Answer, &dns.A{
						Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: uint32(ttl)},
						A:   net.IP(ip.AsSlice()),
					})
				}
			}
		}
		_ = w.WriteMsg(response)
	})
}
