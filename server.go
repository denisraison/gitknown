package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
)

// store caches the discovered repos and serves lookups by id.
type store struct {
	mu    sync.RWMutex
	roots []string
	byID  map[string]Repo
	list  []Repo
}

func newStore(roots []string) *store {
	s := &store{roots: roots, byID: map[string]Repo{}}
	s.refresh()
	return s
}

func (s *store) refresh() {
	repos := discoverRepos(s.roots)
	byID := make(map[string]Repo, len(repos))
	for _, r := range repos {
		byID[r.ID] = r
	}
	s.mu.Lock()
	s.list, s.byID = repos, byID
	s.mu.Unlock()
}

// refreshOne re-reads a single repo's summary in place (after a file change).
func (s *store) refreshOne(id string) {
	s.mu.RLock()
	old, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return
	}
	if r, ok := loadRepo(old.Path, old.Path); ok {
		r.ID, r.Name = old.ID, old.Name // keep stable identity/label
		s.mu.Lock()
		s.byID[id] = r
		for i := range s.list {
			if s.list[i].ID == id {
				s.list[i] = r
			}
		}
		s.mu.Unlock()
	}
}

func (s *store) repos() []Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Repo, len(s.list))
	copy(out, s.list)
	return out
}

func (s *store) get(id string) (Repo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	return r, ok
}

// hub fans repo-changed events out to connected SSE clients.
type hub struct {
	mu      sync.Mutex
	clients map[chan string]bool
}

func newHub() *hub { return &hub{clients: map[chan string]bool{}} }

func (h *hub) add() chan string {
	ch := make(chan string, 8)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *hub) remove(ch chan string) {
	h.mu.Lock()
	delete(h.clients, ch)
	close(ch)
	h.mu.Unlock()
}

func (h *hub) broadcast(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- id:
		default: // slow client, drop
		}
	}
}

type api struct {
	store *store
	hub   *hub
}

func (a *api) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos", a.handleRepos)
	mux.HandleFunc("GET /api/repos/{id}/files", a.handleFiles)
	mux.HandleFunc("GET /api/repos/{id}/tree", a.handleTree)
	mux.HandleFunc("GET /api/repos/{id}/file", a.handleFile)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	return mux
}

func (a *api) handleRepos(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.store.repos())
}

func (a *api) handleFiles(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.store.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such repo", http.StatusNotFound)
		return
	}
	writeJSON(w, changedFiles(repo.Path, repo.Base))
}

func (a *api) handleTree(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.store.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such repo", http.StatusNotFound)
		return
	}
	writeJSON(w, repoTree(repo.Path))
}

func (a *api) handleFile(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.store.get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such repo", http.StatusNotFound)
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	// mode=view: an unchanged context file has no diff, just its contents.
	if r.URL.Query().Get("mode") == "view" {
		writeJSON(w, viewFile(repo.Path, rel))
		return
	}
	status := r.URL.Query().Get("status")
	writeJSON(w, fileDiff(repo.Path, repo.Base, rel, status))
}

// handleEvents streams repo-changed ids as Server-Sent Events.
func (a *api) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := a.hub.add()
	defer a.hub.remove(ch)
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case id := <-ch:
			if _, err := w.Write([]byte("event: changed\ndata: " + id + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("gitknown: encode response: %v", err)
	}
}
