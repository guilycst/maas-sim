package api

import (
	"net/http"

	"github.com/guilycst/maas-sim/internal/seed"
	"github.com/guilycst/maas-sim/internal/store"
)

func (s *Server) tagRoutes() {
	s.route(http.MethodGet, "tags/{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.store.Tags())
	})
	s.route(http.MethodPost, "tags/{$}", s.createTag)
	s.route(http.MethodGet, "tags/{name}/{$}", s.tag, "machines")
	s.route(http.MethodPost, "tags/{name}/{$}", s.updateTagNodes, "update_nodes")
}

func (s *Server) createTag(w http.ResponseWriter, r *http.Request) {
	name := getParam(r, "name")
	if !seed.ValidResourceName(name) {
		fail(w, http.StatusBadRequest, "invalid tag name")
		return
	}
	tag := store.NewTag(name)
	tag.Definition = getParam(r, "definition")
	tag.Comment = getParam(r, "comment")
	tag.KernelOpts = getParam(r, "kernel_opts")
	writeResult(w, http.StatusOK, tag, s.store.AddTag(tag))
}

func (s *Server) tag(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tag, ok := s.store.Tag(name)
	if !ok {
		fail(w, http.StatusNotFound, "tag not found")
		return
	}
	if getParam(r, "op") == "machines" {
		writeJSON(w, http.StatusOK, s.store.Machines(store.MachineFilter{Tags: []string{name}}))
		return
	}
	writeJSON(w, http.StatusOK, tag)
}

func (s *Server) updateTagNodes(w http.ResponseWriter, r *http.Request) {
	if getParam(r, "op") != "update_nodes" {
		fail(w, http.StatusBadRequest, "unknown tag operation")
		return
	}
	add, remove := listParams(r, "add"), listParams(r, "remove")
	err := s.store.UpdateTagNodes(r.PathValue("name"), add, remove)
	writeResult(w, http.StatusOK, map[string]int{"added": len(add), "removed": len(remove)}, err)
}
