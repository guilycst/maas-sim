package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/guilycst/maas-sim/internal/driver"
	"github.com/guilycst/maas-sim/internal/store"
)

const maxFormBytes = 8 << 20

type Server struct {
	ctx        context.Context
	store      *store.Store
	driver     driver.MachineDriver
	mux        *http.ServeMux
	aliases    *http.ServeMux
	operations map[string]*machineOperation
	deploys    sync.WaitGroup
	networkMu  sync.Mutex
	nextIP     uint64
}

type machineOperation struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func New(ctx context.Context, state *store.Store, backend driver.MachineDriver) *Server {
	s := &Server{
		ctx: ctx, store: state, driver: backend, mux: http.NewServeMux(),
		aliases:    http.NewServeMux(),
		operations: make(map[string]*machineOperation),
	}
	for _, machine := range state.Machines(store.MachineFilter{}) {
		s.operations[machine.SystemID] = &machineOperation{}
	}
	s.systemRoutes()
	s.machineRoutes()
	s.networkRoutes()
	s.dnsRoutes()
	s.tagRoutes()
	return s
}

func (s *Server) route(method, pattern string, handler http.HandlerFunc, operations ...string) {
	base := method + " " + store.Prefix
	s.mux.HandleFunc(base+pattern, handler)
	for _, operation := range operations {
		alias := base + strings.TrimSuffix(pattern, "{$}") + "op-" + operation
		withOperation := func(w http.ResponseWriter, r *http.Request) {
			params := r.URL.Query()
			params.Set("op", operation)
			r.URL.RawQuery = params.Encode()
			handler(w, r)
		}
		s.aliases.HandleFunc(alias, withOperation)
		s.aliases.HandleFunc(alias+"/{$}", withOperation)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, store.Prefix) {
		http.NotFound(w, r)
		return
	}
	if token := oauthToken(r.Header.Get("Authorization")); token == "" {
		w.Header().Set("WWW-Authenticate", `OAuth realm="MAAS"`)
		fail(w, http.StatusUnauthorized, "OAuth authorization required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	err := parseForm(r)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		fail(w, http.StatusBadRequest, "invalid form")
		return
	}
	// Prefer an explicit operation on a resource, such as machines/op-allocate/?op=release.
	_, resource := s.mux.Handler(r)
	if resource == "" || getParam(r, "op") == "" {
		if _, alias := s.aliases.Handler(r); alias != "" {
			s.aliases.ServeHTTP(w, r)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// Wait must be called after the HTTP server stops accepting requests.
func (s *Server) Wait() { s.deploys.Wait() }

func oauthToken(header string) string {
	scheme, params, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "OAuth") {
		return ""
	}
	fields := make(map[string]string)
	for _, param := range strings.Split(params, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || key == "" || len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
			return ""
		}
		if _, duplicate := fields[key]; duplicate {
			return ""
		}
		value = value[1 : len(value)-1]
		if strings.ContainsAny(value, "\"\r\n") {
			return ""
		}
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return ""
		}
		fields[key] = decoded
	}
	if fields["oauth_consumer_key"] == "" {
		return ""
	}
	return fields["oauth_token"]
}

func parseForm(r *http.Request) error {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" {
		return r.ParseMultipartForm(1 << 20)
	}
	if r.Method == http.MethodDelete && mediaType == "application/x-www-form-urlencoded" {
		// net/http only reads URL-encoded bodies on POST, PUT and PATCH.
		copy := *r
		copy.Method = http.MethodPost
		err := copy.ParseForm()
		r.Form, r.PostForm = copy.Form, copy.PostForm
		return err
	}
	return r.ParseForm()
}

func getParam(r *http.Request, key string) string {
	if values := r.URL.Query()[key]; len(values) > 0 {
		return values[0]
	}
	if r.MultipartForm != nil {
		if values := r.MultipartForm.Value[key]; len(values) > 0 {
			return values[0]
		}
	}
	return r.FormValue(key)
}

func formValues(r *http.Request, key string) ([]string, bool) {
	if values, ok := r.URL.Query()[key]; ok {
		return values, true
	}
	values, ok := r.PostForm[key]
	return values, ok
}

func listParams(r *http.Request, keys ...string) []string {
	var values []string
	for _, key := range keys {
		items, _ := formValues(r, key)
		for _, item := range items {
			for _, part := range strings.Split(item, ",") {
				if cleaned := strings.TrimSpace(part); cleaned != "" {
					values = append(values, cleaned)
				}
			}
		}
	}
	return values
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Debug("write API response", "error", err)
	}
}

func fail(w http.ResponseWriter, status int, message string) {
	http.Error(w, message, status)
}

func writeResult(w http.ResponseWriter, status int, result any, err error) {
	switch {
	case err == nil:
		writeJSON(w, status, result)
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrInvalid):
		fail(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrConflict):
		fail(w, http.StatusConflict, err.Error())
	default:
		slog.Error("MAAS operation failed", "error", err)
		fail(w, http.StatusBadGateway, "backend operation failed")
	}
}

func backendContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
}
