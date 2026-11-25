package api

import (
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/canonical/gomaasclient/entity"

	"github.com/guilycst/maas-sim/internal/dnsname"
	"github.com/guilycst/maas-sim/internal/driver"
	"github.com/guilycst/maas-sim/internal/store"
)

func (s *Server) networkRoutes() {
	topology := s.store.Topology()
	s.route(http.MethodGet, "fabrics/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []entity.Fabric{topology.Fabric})
	})
	vlans := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []struct {
			entity.VLAN
			VID int `json:"vid"`
		}{{VLAN: topology.VLAN, VID: topology.VLAN.VID}})
	}
	s.route(http.MethodGet, "vlans/{$}", vlans)
	s.route(http.MethodGet, "fabrics/"+strconv.Itoa(topology.Fabric.ID)+"/vlans/{$}", vlans)
	s.route(http.MethodGet, "subnets/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []entity.Subnet{topology.Subnet})
	})
	s.route(http.MethodGet, "ipranges/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []entity.IPRange{topology.Range})
	})
	s.route(http.MethodGet, "ipaddresses/{$}", s.ipAddresses)
	s.route(http.MethodPost, "ipaddresses/{$}", s.ipAction, "reserve", "release")
}

func (s *Server) matchesSubnet(selector string) bool {
	subnet := s.store.Topology().Subnet
	return selector == "" || selector == subnet.CIDR || selector == strconv.Itoa(subnet.ID) || selector == subnet.Name
}

func (s *Server) ipAddresses(w http.ResponseWriter, r *http.Request) {
	addresses := []entity.IPAddress{}
	if s.matchesSubnet(getParam(r, "subnet")) {
		ip, owner := getParam(r, "ip"), getParam(r, "owner")
		for _, address := range s.store.IPAddresses() {
			if (ip == "" || ip == address.IP.String()) && (owner == "" || owner == address.Owner.UserName) {
				addresses = append(addresses, address)
			}
		}
	}
	writeJSON(w, http.StatusOK, addresses)
}

func (s *Server) ipAction(w http.ResponseWriter, r *http.Request) {
	if !s.matchesSubnet(getParam(r, "subnet")) {
		fail(w, http.StatusNotFound, "subnet not found")
		return
	}
	requested := getParam(r, "ip")
	var ip netip.Addr
	if requested != "" {
		var err error
		ip, err = netip.ParseAddr(requested)
		if err != nil || !s.store.ValidIP(ip) {
			fail(w, http.StatusBadRequest, "ip is outside the usable IPv4 subnet")
			return
		}
	}
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	switch getParam(r, "op") {
	case "reserve":
		s.reserveIP(w, r, ip)
	case "release":
		s.releaseIP(w, r, ip)
	default:
		fail(w, http.StatusBadRequest, "unknown IP address operation")
	}
}

func (s *Server) reserveIP(w http.ResponseWriter, r *http.Request, ip netip.Addr) {
	host := strings.TrimSuffix(strings.ToLower(getParam(r, "hostname")), ".")
	if host != "" && !dnsname.Valid(host) {
		fail(w, http.StatusBadRequest, "invalid reservation hostname")
		return
	}
	if host != "" && !strings.Contains(host, ".") {
		host += "." + s.store.Domain().Name
	}
	if existing, ok := s.store.ReservedIP(ip); ok {
		writeJSON(w, http.StatusOK, existing.Address)
		return
	}
	if host != "" {
		if _, _, exists := s.store.Lookup(host); exists {
			fail(w, http.StatusConflict, "reservation hostname already exists")
			return
		}
	}
	var leaseID string
	if !s.isMachineIP(ip) {
		s.nextIP++
		leaseID = strconv.FormatUint(s.nextIP, 10)
		ctx, cancel := backendContext(r)
		defer cancel()
		var err error
		ip, err = s.driver.ReserveIP(ctx, driver.IPLease{ID: leaseID, Hostname: host, IP: ip})
		if err != nil {
			writeResult(w, http.StatusOK, nil, err)
			return
		}
	}
	address := s.store.IPAddress(ip, "User reserved")
	s.store.SaveIP(ip, store.IPReservation{Address: address, Hostname: host, LeaseID: leaseID})
	writeJSON(w, http.StatusOK, address)
}

func (s *Server) releaseIP(w http.ResponseWriter, r *http.Request, ip netip.Addr) {
	if !ip.IsValid() {
		fail(w, http.StatusBadRequest, "ip is required")
		return
	}
	reservation, ok := s.store.ReservedIP(ip)
	if !ok {
		writeResult(w, http.StatusOK, nil, fmt.Errorf("%w: reserved ip %s", store.ErrNotFound, ip))
		return
	}
	if reservation.LeaseID != "" {
		ctx, cancel := backendContext(r)
		defer cancel()
		if err := s.driver.ReleaseIP(ctx, reservation.LeaseID); err != nil {
			writeResult(w, http.StatusOK, nil, err)
			return
		}
	}
	s.store.ReleaseIP(ip)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) isMachineIP(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	for _, address := range s.store.IPAddresses() {
		if address.IP.String() == ip.String() {
			return true
		}
	}
	return false
}
