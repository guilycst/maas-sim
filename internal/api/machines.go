package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/canonical/gomaasclient/entity"
	"github.com/canonical/gomaasclient/entity/node"

	"github.com/guilycst/maas-sim/internal/dnsname"
	"github.com/guilycst/maas-sim/internal/driver"
	"github.com/guilycst/maas-sim/internal/store"
)

func (s *Server) machineRoutes() {
	s.route(http.MethodGet, "machines/{$}", s.machines, "list_allocated")
	s.route(http.MethodPost, "machines/{$}", s.allocate, "allocate")
	s.route(http.MethodGet, "machines/{id}/{$}", s.machine, "query_power_state")
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		s.route(method, "machines/{id}/{$}", s.machineAction,
			"deploy", "release", "power_on", "power_off", "query_power_state")
	}
	s.route(http.MethodGet, "machines/{id}/interfaces/{$}", s.interfaces)
}

func parseMachineFilter(r *http.Request) (store.MachineFilter, error) {
	filter := store.MachineFilter{
		Hostnames: listParams(r, "hostname", "name"), IDs: listParams(r, "id", "system_id"),
		Tags: listParams(r, "tags", "tag"), NotTags: listParams(r, "not_tags"),
		Zones: listParams(r, "zone"), Pools: listParams(r, "pool"),
	}
	var err error
	if filter.CPU, err = minimum(r, "cpu_count", "cpu"); err != nil {
		return filter, err
	}
	if filter.Memory, err = minimum(r, "mem", "memory"); err != nil {
		return filter, err
	}
	for _, status := range listParams(r, "status") {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "ready":
			filter.Statuses = append(filter.Statuses, node.StatusReady)
		case "allocated":
			filter.Statuses = append(filter.Statuses, node.StatusAllocated)
		case "deploying":
			filter.Statuses = append(filter.Statuses, node.StatusDeploying)
		case "deployed":
			filter.Statuses = append(filter.Statuses, node.StatusDeployed)
		case "releasing":
			filter.Statuses = append(filter.Statuses, node.StatusReleasing)
		case "failed deployment":
			filter.Statuses = append(filter.Statuses, node.StatusFailedDeployment)
		default:
			code, err := strconv.Atoi(status)
			if err != nil || code < 0 {
				return filter, fmt.Errorf("invalid status %q", status)
			}
			filter.Statuses = append(filter.Statuses, node.Status(code))
		}
	}
	return filter, nil
}

func minimum(r *http.Request, keys ...string) (int64, error) {
	for _, key := range keys {
		value := getParam(r, key)
		if value == "" {
			continue
		}
		// MAAS accepts minimum constraints like "4" and "4.0".
		number, err := strconv.ParseInt(strings.TrimSuffix(value, ".0"), 10, 64)
		if err != nil || number < 0 {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return number, nil
	}
	return 0, nil
}

func (s *Server) machines(w http.ResponseWriter, r *http.Request) {
	op := getParam(r, "op")
	if op != "" && op != "list_allocated" {
		fail(w, http.StatusBadRequest, "unknown machine list operation")
		return
	}
	filter, err := parseMachineFilter(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if op == "list_allocated" {
		filter.Statuses = []node.Status{node.StatusAllocated, node.StatusDeploying, node.StatusDeployed}
	}
	writeJSON(w, http.StatusOK, s.store.Machines(filter))
}

func (s *Server) allocate(w http.ResponseWriter, r *http.Request) {
	if getParam(r, "op") != "allocate" {
		fail(w, http.StatusBadRequest, "unknown machine collection operation")
		return
	}
	filter, err := parseMachineFilter(r)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	machine, err := s.store.Allocate(filter, getParam(r, "dry_run") == "true")
	writeResult(w, http.StatusOK, machine, err)
}

func (s *Server) machine(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.store.Machine(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, "machine not found")
		return
	}
	switch getParam(r, "op") {
	case "":
		writeJSON(w, http.StatusOK, machine)
	case "query_power_state":
		writeJSON(w, http.StatusOK, entity.MachinePowerState{State: machine.PowerState})
	default:
		fail(w, http.StatusBadRequest, "unknown machine operation")
	}
}

func (s *Server) interfaces(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.store.Machine(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, "machine not found")
		return
	}
	writeJSON(w, http.StatusOK, machine.InterfaceSet)
}

func (s *Server) machineAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	operation, ok := s.operations[id]
	if !ok {
		fail(w, http.StatusNotFound, "machine not found")
		return
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()

	if err := s.ctx.Err(); err != nil {
		fail(w, http.StatusServiceUnavailable, "simulator is shutting down")
		return
	}
	action := getParam(r, "op")
	switch {
	case r.Method == http.MethodPut && action == "":
		s.updateMachine(w, r, id)
	case action == "deploy":
		s.deploy(w, r, id, operation)
	case action == "release":
		s.release(w, r, id, operation)
	case action == "power_on" || action == "power_off":
		s.power(w, r, id, action == "power_on")
	case action == "query_power_state":
		s.machine(w, r)
	default:
		fail(w, http.StatusBadRequest, "unknown machine operation")
	}
}

func (s *Server) updateMachine(w http.ResponseWriter, r *http.Request, id string) {
	swap, err := minimum(r, "swap_size")
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	hostname := getParam(r, "hostname")
	if hostname != "" && !dnsname.ValidLabel(hostname) {
		fail(w, http.StatusBadRequest, "invalid hostname")
		return
	}
	machine, err := s.store.ChangeMachine(id, func(m *entity.Machine) error {
		if hostname != "" && m.Hostname != hostname {
			if m.Status != node.StatusReady && m.Status != node.StatusAllocated {
				return store.ErrConflict
			}
			m.Hostname, m.FQDN = hostname, hostname+"."+m.Domain.Name
		}
		if _, hasSwap := formValues(r, "swap_size"); hasSwap {
			m.SwapSize = swap
		}
		return nil
	})
	writeResult(w, http.StatusOK, machine, err)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request, id string, operation *machineOperation) {
	userData, err := base64.StdEncoding.DecodeString(getParam(r, "user_data"))
	if err != nil {
		fail(w, http.StatusBadRequest, "user_data must be standard base64")
		return
	}
	machine, err := s.store.ChangeMachine(id, func(m *entity.Machine) error {
		switch m.Status {
		case node.StatusDeploying, node.StatusDeployed:
			return nil
		case node.StatusAllocated:
			store.SetStatus(m, node.StatusDeploying, "Deploying")
			m.PowerState = "on"
			m.DistroSeries = getParam(r, "distro_series")
			m.OSystem = getParam(r, "osystem")
			m.HWEKernel = getParam(r, "hwe_kernel")
			m.EphemeralDeploy, _ = strconv.ParseBool(getParam(r, "ephemeral_deploy"))
			return nil
		default:
			return fmt.Errorf("%w: allocate the machine before deploying", store.ErrConflict)
		}
	})
	if err != nil || machine.Status == node.StatusDeployed || operation.done != nil {
		writeResult(w, http.StatusOK, machine, err)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	done := make(chan struct{})
	operation.cancel, operation.done = cancel, done
	s.deploys.Add(1)
	go func() {
		defer s.deploys.Done()
		defer close(done)
		defer cancel()
		s.deployNode(ctx, driver.Node{ID: id, Hostname: machine.Hostname, UserData: userData})
	}()
	writeJSON(w, http.StatusOK, machine)
}

func (s *Server) deployNode(ctx context.Context, request driver.Node) {
	instance, err := s.driver.Deploy(ctx, request)
	_, updateErr := s.store.ChangeMachine(request.ID, func(m *entity.Machine) error {
		if m.Status != node.StatusDeploying {
			return nil
		}
		if err != nil {
			store.SetStatus(m, node.StatusFailedDeployment, "Failed deployment")
			m.PowerState = "off"
			m.StatusMessage = "Backend deployment failed"
			return nil
		}
		store.SetStatus(m, node.StatusDeployed, "Deployed")
		s.store.SetInstance(m, instance)
		return nil
	})
	if failure := errors.Join(err, updateErr); failure != nil && !errors.Is(failure, context.Canceled) {
		slog.Error("deploy machine", "system_id", request.ID, "error", failure)
	}
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, id string, operation *machineOperation) {
	machine, _ := s.store.Machine(id)
	if machine.Status == node.StatusReady {
		writeJSON(w, http.StatusOK, machine)
		return
	}
	machine, err := s.store.ChangeMachine(id, func(m *entity.Machine) error {
		store.SetStatus(m, node.StatusReleasing, "Releasing")
		return nil
	})
	if err != nil {
		writeResult(w, http.StatusOK, machine, err)
		return
	}
	ctx, cancel := backendContext(r)
	defer cancel()
	if operation.done != nil {
		operation.cancel()
		select {
		case <-operation.done:
			operation.cancel, operation.done = nil, nil
		case <-ctx.Done():
			writeResult(w, http.StatusOK, nil, ctx.Err())
			return
		}
	}

	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	if err := s.driver.Release(ctx, id); err != nil {
		writeResult(w, http.StatusOK, nil, err)
		return
	}
	for _, ip := range machine.IPAddresses {
		address, _ := netip.AddrFromSlice(ip)
		s.detachAddress(address.Unmap())
		s.store.ReleaseIP(address.Unmap())
	}
	machine, err = s.store.ChangeMachine(id, func(m *entity.Machine) error {
		s.store.ResetMachine(m)
		return nil
	})
	writeResult(w, http.StatusOK, machine, err)
}

func (s *Server) power(w http.ResponseWriter, r *http.Request, id string, on bool) {
	machine, _ := s.store.Machine(id)
	if machine.Status != node.StatusDeployed {
		writeResult(w, http.StatusOK, nil, fmt.Errorf("%w: only deployed machines can change power state", store.ErrConflict))
		return
	}
	ctx, cancel := backendContext(r)
	defer cancel()
	s.networkMu.Lock()
	defer s.networkMu.Unlock()
	instance, err := s.driver.Power(ctx, id, on)
	if err != nil {
		writeResult(w, http.StatusOK, nil, err)
		return
	}
	// Docker can assign a different dynamically allocated address after a stop.
	for _, ip := range machine.IPAddresses {
		previous, _ := netip.AddrFromSlice(ip)
		previous = previous.Unmap()
		if previous != instance.IP {
			s.detachAddress(previous)
			s.store.ReleaseIP(previous)
		}
	}
	machine, err = s.store.ChangeMachine(id, func(m *entity.Machine) error {
		m.PowerState = "off"
		if on {
			m.PowerState = "on"
		}
		s.store.SetInstance(m, instance)
		return nil
	})
	writeResult(w, http.StatusOK, machine, err)
}
