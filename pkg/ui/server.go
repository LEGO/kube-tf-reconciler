package ui

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed assets/index.html
var staticFiles embed.FS

// state holds the mutable active client and namespace, protected by a mutex
// so the poller goroutine and HTTP handlers can read it concurrently while
// /api/switch replaces it.
type state struct {
	mu        sync.RWMutex
	k8sClient client.Client
	namespace string
	context   string
}

func (s *state) get() (client.Client, string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.k8sClient, s.namespace, s.context
}

func (s *state) set(k8sClient client.Client, namespace, context string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.k8sClient = k8sClient
	s.namespace = namespace
	s.context = context
}

func Run(ctx context.Context, initialClient client.Client, namespace string, initialContext string, port int) error {
	if initialContext == "" {
		_, currentContext, err := listKubeContexts()
		if err != nil {
			slog.Error("failed to derive initial kube context", "error", err)
		} else {
			initialContext = currentContext
		}
	}

	st := &state{k8sClient: initialClient, namespace: namespace, context: initialContext}
	b := newBroker()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := staticFiles.ReadFile("assets/index.html")
		if err != nil {
			slog.Error("failed to read embedded UI asset", "path", "assets/index.html", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})

	mux.HandleFunc("GET /api/contexts", func(w http.ResponseWriter, r *http.Request) {
		ctxList, current, err := listKubeContexts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _, activeCtx := st.get()
		if activeCtx != "" {
			current = activeCtx
		}
		writeJSON(w, map[string]any{"contexts": ctxList, "current": current})
	})

	mux.HandleFunc("GET /api/namespaces", func(w http.ResponseWriter, r *http.Request) {
		k8sClient, _, _ := st.get()
		ctx2, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		namespaces, err := listNamespaces(ctx2, k8sClient)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, activeNS, _ := st.get()
		writeJSON(w, map[string]any{"namespaces": namespaces, "current": activeNS})
	})

	mux.HandleFunc("POST /api/switch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Context   string `json:"context"`
			Namespace string `json:"namespace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		newClient, err := buildClientForContext(req.Context)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to build client for context %q: %s", req.Context, err), http.StatusBadRequest)
			return
		}

		st.set(newClient, req.Namespace, req.Context)
		slog.Info("switched context/namespace", "context", req.Context, "namespace", req.Namespace)

		// Immediately push a fresh workspace list to all SSE subscribers.
		k8sClient, ns, _ := st.get()
		go pollAndBroadcast(ctx, k8sClient, ns, b)

		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/workspaces", func(w http.ResponseWriter, r *http.Request) {
		k8sClient, ns, _ := st.get()
		ctx2, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		workspaces, err := fetchWorkspaces(ctx2, k8sClient, ns)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, workspaces)
	})

	mux.HandleFunc("GET /api/workspaces/{namespace}/{name}/plans", func(w http.ResponseWriter, r *http.Request) {
		k8sClient, _, _ := st.get()
		ns := r.PathValue("namespace")
		name := r.PathValue("name")
		ctx2, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		plans, err := listWorkspacePlans(ctx2, k8sClient, ns, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, plans)
	})

	mux.HandleFunc("GET /api/workspaces/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		k8sClient, _, _ := st.get()
		ns := r.PathValue("namespace")
		name := r.PathValue("name")
		ctx2, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		detail, err := fetchWorkspaceDetail(ctx2, k8sClient, ns, name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, detail)
	})

	mux.HandleFunc("GET /api/events", b.ServeSSE)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		k8sClient, ns, _ := st.get()
		pollAndBroadcast(ctx, k8sClient, ns, b)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				k8sClient, ns, _ := st.get()
				pollAndBroadcast(ctx, k8sClient, ns, b)
			}
		}
	}()

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	url := fmt.Sprintf("http://localhost:%d", port)
	slog.Info("starting web UI", "url", url)
	go func() {
		time.Sleep(200 * time.Millisecond)
		openBrowser(url)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func pollAndBroadcast(ctx context.Context, k8sClient client.Client, namespace string, b *broker) {
	pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	workspaces, err := fetchWorkspaces(pollCtx, k8sClient, namespace)
	if err != nil {
		slog.Warn("polling workspaces failed", "error", err)
		return
	}
	payload := map[string]any{"type": "workspaces", "workspaces": workspaces}
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("marshalling SSE payload failed", "error", err)
		return
	}
	b.broadcast(string(data))
}

// buildClientForContext creates a new controller-runtime client for the named kubeconfig context.
func buildClientForContext(kubeContext string) (client.Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: internalScheme})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Warn("writing JSON response failed", "error", err)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("could not open browser", "error", err)
	}
}
