// Package server exposes the store over HTTP: a bearer-token-authenticated
// JSON API under /api/v1, an unauthenticated /healthz, and a background
// backup scheduler.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mikedclarke/trackd/internal/store"
)

type Server struct {
	store   *store.Store
	version string

	mu         sync.Mutex
	lastBackup backupStatus
}

type backupStatus struct {
	At    time.Time `json:"at,omitzero"`
	Path  string    `json:"path,omitempty"`
	Error string    `json:"error,omitempty"`
}

func New(st *store.Store, version string) *Server {
	return &Server{store: st, version: version}
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/issues", s.handleListIssues)
	api.HandleFunc("POST /api/v1/issues", s.handleCreateIssue)
	api.HandleFunc("GET /api/v1/issues/{key}", s.handleGetIssue)
	api.HandleFunc("PATCH /api/v1/issues/{key}", s.handlePatchIssue)
	api.HandleFunc("GET /api/v1/issues/{key}/comments", s.handleListComments)
	api.HandleFunc("POST /api/v1/issues/{key}/comments", s.handleAddComment)
	api.HandleFunc("GET /api/v1/issues/{key}/relations", s.handleListRelations)
	api.HandleFunc("POST /api/v1/issues/{key}/relations", s.handleSetRelation)
	api.HandleFunc("GET /api/v1/issues/{key}/events", s.handleListEvents)
	api.HandleFunc("GET /api/v1/projects", s.handleListProjects)
	api.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	api.HandleFunc("GET /api/v1/projects/{slug}", s.handleGetProject)
	api.HandleFunc("PATCH /api/v1/projects/{slug}", s.handlePatchProject)
	api.HandleFunc("GET /api/v1/labels", s.handleListLabels)
	api.HandleFunc("POST /api/v1/labels", s.handleEnsureLabel)
	api.HandleFunc("GET /api/v1/statuses", s.handleListStatuses)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", s.auth(api))
	mux.Handle("/mcp", s.auth(s.mcpHandler()))
	return logRequests(mux)
}

type ctxKey int

const tokenKey ctxKey = 0

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		plaintext, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || plaintext == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token, err := s.store.VerifyToken(plaintext)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, token)))
	})
}

// actor resolves attribution for a write: an explicit actor in the request
// body wins, otherwise the authenticating token's name is used.
func actor(r *http.Request, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if t, ok := r.Context().Value(tokenKey).(*store.Token); ok {
		return t.Name
	}
	return ""
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK
	if err := s.store.Ping(); err != nil {
		status = "degraded"
		code = http.StatusInternalServerError
	}
	s.mu.Lock()
	backup := s.lastBackup
	s.mu.Unlock()
	body := map[string]any{
		"status":  status,
		"version": s.version,
	}
	if !backup.At.IsZero() || backup.Error != "" {
		info := map[string]any{}
		if !backup.At.IsZero() {
			info["last_at"] = backup.At.UTC().Format(time.RFC3339)
			info["age_seconds"] = int(time.Since(backup.At).Seconds())
			info["path"] = backup.Path
		}
		if backup.Error != "" {
			info["error"] = backup.Error
		}
		body["backup"] = info
	}
	writeJSON(w, code, body)
}

type BackupConfig struct {
	Dir   string
	Every time.Duration
	Keep  int
}

// RunBackups snapshots the database immediately and then on every tick until
// ctx is canceled. Failures are recorded for /healthz and logged, never fatal.
func (s *Server) RunBackups(ctx context.Context, cfg BackupConfig) {
	if cfg.Dir == "" {
		return
	}
	run := func() {
		path, err := s.store.Backup(cfg.Dir)
		status := backupStatus{At: time.Now(), Path: path}
		if err != nil {
			status = backupStatus{Error: err.Error()}
			log.Printf("backup failed: %v", err)
		} else {
			log.Printf("backup written: %s", path)
			if cfg.Keep > 0 {
				if _, err := store.PruneBackups(cfg.Dir, cfg.Keep); err != nil {
					log.Printf("backup prune failed: %v", err)
				}
			}
		}
		s.mu.Lock()
		s.lastBackup = status
		s.mu.Unlock()
	}
	run()
	ticker := time.NewTicker(cfg.Every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeStoreError maps store failures onto HTTP statuses: unknown entities are
// 404, anything else on a request the client shaped is 400.
func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// decodeBody strictly decodes a JSON request body, rejecting unknown fields so
// agent typos fail loudly instead of being silently ignored.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}
