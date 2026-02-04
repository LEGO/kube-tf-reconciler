package testutils

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type ModuleEvent string

const (
	ModuleEventNothing ModuleEvent = "nothing"
	ModuleEventApply   ModuleEvent = "apply"
	ModuleEventDestroy ModuleEvent = "destroy"
)

type ModuleStateServer struct {
	server *httptest.Server
	events map[string][]ModuleEvent
	mu     *sync.Mutex
}

func NewModuleStateServer(t *testing.T) *ModuleStateServer {
	srv := &ModuleStateServer{
		events: make(map[string][]ModuleEvent),
		mu:     &sync.Mutex{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		srv.mu.Lock()
		srv.events[name] = append(srv.events[name], ModuleEventApply)
		srv.mu.Unlock()
		slog.Info("created module", "module", name)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /destroy", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		srv.mu.Lock()
		srv.events[name] = append(srv.events[name], ModuleEventDestroy)
		srv.mu.Unlock()
		slog.Info("destroyed module", "module", name)
		w.WriteHeader(http.StatusOK)
	})
	srv.server = httptest.NewServer(mux)
	return srv
}

func (s *ModuleStateServer) Close() {
	s.server.Close()
}

func (s *ModuleStateServer) URL() string {
	return s.server.URL
}

func (s *ModuleStateServer) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = make(map[string][]ModuleEvent)
}

func (s *ModuleStateServer) CurrentStatus(name string) ModuleEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events[name]) == 0 {
		return ModuleEventNothing
	}
	return s.events[name][len(s.events[name])-1]
}

func (s *ModuleStateServer) GetModule(name string) string {
	module := fmt.Sprintf(`variable "pet_name_length" {
  default = 2
  type    = number
}

resource "random_pet" "name" {
	length    = var.pet_name_length
	separator = "-"
	provisioner "local-exec" {
		when    = "create"
		command = "curl -XPOST %s/apply?name=%s"
	}
	provisioner "local-exec" {
		when    = "destroy"
		command = "curl -XPOST %s/destroy?name=%s"
	}
}
`, s.URL(), name, s.URL(), name)

	return module
}
