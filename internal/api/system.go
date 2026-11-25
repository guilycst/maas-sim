package api

import (
	"net/http"
	"strconv"

	"github.com/canonical/gomaasclient/entity"

	"github.com/guilycst/maas-sim/internal/dnsname"
	"github.com/guilycst/maas-sim/internal/store"
)

func (s *Server) systemRoutes() {
	s.route(http.MethodGet, "version/{$}", version)
	s.route(http.MethodGet, "users/{$}", whoami, "whoami")
	s.route(http.MethodGet, "domains/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []entity.Domain{s.store.Domain()})
	})
	s.route(http.MethodGet, "domains/{id}/{$}", s.domain)
	s.route(http.MethodPost, "domains/{id}/{$}", s.domain, "set_default")
	s.route(http.MethodPut, "domains/{id}/{$}", s.domain, "set_default")
	s.route(http.MethodGet, "zones/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.store.Topology().Zones)
	})
	s.route(http.MethodGet, "resourcepools/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.store.Topology().Pools)
	})
}

func version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		entity.Version
		Subversion string `json:"subversion"`
	}{
		Version: entity.Version{
			Version:      "3.5.0",
			Capabilities: []string{"static-ipaddresses"},
		},
	})
}

func whoami(w http.ResponseWriter, r *http.Request) {
	if getParam(r, "op") != "whoami" {
		fail(w, http.StatusBadRequest, "unknown user operation")
		return
	}
	writeJSON(w, http.StatusOK, store.Admin)
}

func (s *Server) domain(w http.ResponseWriter, r *http.Request) {
	current := s.store.Domain()
	if r.PathValue("id") != strconv.Itoa(current.ID) {
		fail(w, http.StatusNotFound, "domain not found")
		return
	}
	if r.Method == http.MethodGet || getParam(r, "op") == "set_default" {
		writeJSON(w, http.StatusOK, current)
		return
	}
	name := getParam(r, "name")
	if name != "" && !dnsname.Valid(name) {
		fail(w, http.StatusBadRequest, "invalid domain name")
		return
	}
	ttl, err := minimum(r, "ttl")
	if err != nil || ttl > maxTTL {
		fail(w, http.StatusBadRequest, "invalid domain ttl")
		return
	}
	value, hasAuthoritative := formValues(r, "authoritative")
	var authoritative bool
	if hasAuthoritative {
		authoritative, err = strconv.ParseBool(value[0])
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid authoritative value")
			return
		}
	}
	updated, err := s.store.SetDomain(func(domain *entity.Domain) {
		if name != "" {
			domain.Name = name
		}
		if _, ok := formValues(r, "ttl"); ok {
			domain.TTL = int(ttl)
		}
		if hasAuthoritative {
			domain.Authoritative = authoritative
		}
	})
	writeResult(w, http.StatusOK, updated, err)
}
