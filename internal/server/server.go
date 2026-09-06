// Package server exposes the store over HTTP: a bearer-token-authenticated
// JSON API under /api/v1, an unauthenticated /healthz, an MCP endpoint, a
// read-only web UI, and the background backup and integrity checkers.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	"github.com/mikedclarke/trackd/internal/store"
)

// Timeouts the API expects on the http.Server that wraps Handler. They live
// here so the transport settings travel with the handler they protect; main
// owns the http.Server and wires them.
const (
	WriteTimeout   = 60 * time.Second
	IdleTimeout    = 120 * time.Second
	ShutdownBudget = 10 * time.Second
)

// schemaVersion is the store's migration count, reported by /healthz so a
// client can tell which contract it is talking to. The store does not expose
// the number; bump this when a migration is added (latest is
// internal/store/migrations/0003_cutover.sql).
const schemaVersion = 3

// healthCheckEvery is how often the background checker runs PRAGMA
// quick_check. Cheap on a database this size, and rare enough that a wedged
// filesystem shows up in /healthz within ten minutes rather than never.
const healthCheckEvery = 10 * time.Minute

// defaultBackupEvery is the fallback when the configured interval is zero or
// negative: a mistyped flag must not mean "back up in a tight loop".
const defaultBackupEvery = 24 * time.Hour

type Server struct {
	store   *store.Store
	version string

	mu            sync.Mutex
	lastBackup    backupStatus
	integrity     integrityStatus
	backupDir     string
	backupEvery   time.Duration
	healthRunning bool
	sessions      map[string]string
}

type backupStatus struct {
	At    time.Time
	Error string
}

type integrityStatus struct {
	At time.Time
	OK bool
}

func New(st *store.Store, version string) *Server {
	return &Server{store: st, version: version, sessions: map[string]string{}}
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/issues", s.handleListIssues)
	api.HandleFunc("POST /api/v1/issues", s.handleCreateIssue)
	api.HandleFunc("GET /api/v1/issues/{key}", s.handleGetIssue)
	api.HandleFunc("PATCH /api/v1/issues/{key}", s.handlePatchIssue)
	api.HandleFunc("POST /api/v1/issues/{key}/description", s.handleAppendDescription)
	api.HandleFunc("GET /api/v1/issues/{key}/comments", s.handleListComments)
	api.HandleFunc("POST /api/v1/issues/{key}/comments", s.handleAddComment)
	api.HandleFunc("PATCH /api/v1/comments/{id}", s.handlePatchComment)
	api.HandleFunc("GET /api/v1/issues/{key}/relations", s.handleListRelations)
	api.HandleFunc("POST /api/v1/issues/{key}/relations", s.handleSetRelation)
	api.HandleFunc("GET /api/v1/issues/{key}/events", s.handleListIssueEvents)
	api.HandleFunc("GET /api/v1/events", s.handleListEvents)
	api.HandleFunc("GET /api/v1/projects", s.handleListProjects)
	api.HandleFunc("POST /api/v1/projects", s.handleCreateProject)
	api.HandleFunc("GET /api/v1/projects/{slug}", s.handleGetProject)
	api.HandleFunc("PATCH /api/v1/projects/{slug}", s.handlePatchProject)
	api.HandleFunc("GET /api/v1/projects/{slug}/events", s.handleListProjectEvents)
	api.HandleFunc("GET /api/v1/milestones", s.handleListMilestones)
	api.HandleFunc("POST /api/v1/milestones", s.handleCreateMilestone)
	api.HandleFunc("GET /api/v1/milestones/{id}", s.handleGetMilestone)
	api.HandleFunc("PATCH /api/v1/milestones/{id}", s.handlePatchMilestone)
	api.HandleFunc("GET /api/v1/milestones/{id}/events", s.handleListMilestoneEvents)
	api.HandleFunc("GET /api/v1/labels", s.handleListLabels)
	api.HandleFunc("POST /api/v1/labels", s.handleEnsureLabel)
	api.HandleFunc("GET /api/v1/statuses", s.handleListStatuses)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("/api/", s.auth(api))
	mux.Handle("/mcp", s.auth(s.mcpHandler()))
	mux.HandleFunc("GET /{$}", s.uiAuth(s.uiBoard))
	mux.HandleFunc("GET /ui/issue/{key}", s.uiAuth(s.uiIssue))
	mux.HandleFunc("GET /ui/login", s.uiLoginForm)
	mux.HandleFunc("POST /ui/login", s.uiLoginSubmit)
	mux.HandleFunc("GET /ui/logout", s.uiLogout)
	mux.HandleFunc("GET /ui/static/style.css", s.uiStyle)

	// The integrity checker is half of the health contract, so it starts with
	// the handler rather than waiting for a caller to remember it.
	if s.healthStart() {
		s.checkIntegrity()
		go s.tickHealth(context.Background())
	}
	return logRequests(jsonMuxErrors(mux))
}

type ctxKey int

const (
	tokenKey ctxKey = iota
	requestKey
)

// requestInfo carries what the log line needs but only the inner handlers
// know. One request runs on one goroutine, so it needs no lock.
type requestInfo struct{ actor string }

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		plaintext, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || plaintext == "" {
			writeAuthError(w, "missing bearer token")
			return
		}
		token, err := s.store.VerifyToken(plaintext)
		if err != nil {
			writeAuthError(w, "invalid token")
			return
		}
		if info, ok := r.Context().Value(requestKey).(*requestInfo); ok {
			info.actor = token.Name
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey, token)))
	})
}

func writeAuthError(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, codeUnauthorized, msg)
}

// roleAdmin is the token role the store records for a privileged identity;
// every other token is an agent.
const roleAdmin = "admin"

// tokenRole is the authenticating token's role. Every route that reaches a
// handler sits behind auth, so an absent token is not a caller with fewer
// rights, it is a programming error, and it reads as the least privilege
// there is.
func tokenRole(r *http.Request) string {
	if t, ok := r.Context().Value(tokenKey).(*store.Token); ok {
		return t.Role
	}
	return ""
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

type healthResponse struct {
	Status    string          `json:"status"`
	Version   string          `json:"version"`
	Schema    int             `json:"schema"`
	Backup    healthBackup    `json:"backup"`
	Integrity healthIntegrity `json:"integrity"`
}

// healthBackup deliberately carries no filesystem path: /healthz is
// unauthenticated, and where the snapshots live is not the public's business.
type healthBackup struct {
	LastAt     string `json:"last_at"`
	AgeSeconds *int   `json:"age_seconds"`
	Error      string `json:"error,omitempty"`
}

type healthIntegrity struct {
	CheckedAt string `json:"checked_at"`
	OK        bool   `json:"ok"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	body := healthResponse{Status: "ok", Version: s.version, Schema: schemaVersion}
	degraded := s.store.Ping() != nil

	s.mu.Lock()
	backup, integrity := s.lastBackup, s.integrity
	dir, every := s.backupDir, s.backupEvery
	s.mu.Unlock()

	body.Integrity = healthIntegrity{OK: integrity.OK}
	if !integrity.At.IsZero() {
		body.Integrity.CheckedAt = integrity.At.UTC().Format(time.RFC3339)
	}
	if !integrity.OK {
		degraded = true
	}
	body.Backup.Error = backup.Error
	if backup.Error != "" {
		degraded = true
	}
	if !backup.At.IsZero() {
		age := int(time.Since(backup.At).Seconds())
		body.Backup.LastAt = backup.At.UTC().Format(time.RFC3339)
		body.Backup.AgeSeconds = &age
		// Twice the interval means a tick was missed outright, not that one
		// ran a few seconds late.
		if every > 0 && time.Since(backup.At) > 2*every {
			degraded = true
		}
	}
	// No scheduler at all is the worst backup state there is, so it reads as
	// degraded even though nothing has failed.
	if dir == "" {
		degraded = true
	}

	code := http.StatusOK
	if degraded {
		body.Status = "degraded"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

// healthStart reports whether this caller is the one that starts the
// background integrity checker; a second caller is told to stand down.
func (s *Server) healthStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.healthRunning {
		return false
	}
	s.healthRunning = true
	return true
}

// RunHealth runs the integrity check now and every ten minutes until ctx is
// canceled. Handler already starts the checker on a background context, so
// this is only for a caller that wants to own its lifetime, and it has to be
// called before Handler to win: the second starter returns immediately.
func (s *Server) RunHealth(ctx context.Context) {
	if !s.healthStart() {
		return
	}
	s.checkIntegrity()
	s.tickHealth(ctx)
}

func (s *Server) tickHealth(ctx context.Context) {
	ticker := time.NewTicker(healthCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkIntegrity()
		}
	}
}

func (s *Server) checkIntegrity() {
	err := s.store.QuickCheck()
	s.mu.Lock()
	s.integrity = integrityStatus{At: time.Now(), OK: err == nil}
	s.mu.Unlock()
	if err != nil {
		log.Printf("integrity check failed: %v", err)
	}
}

type BackupConfig struct {
	Dir   string
	Every time.Duration
	Keep  int
	// Timeout bounds each backup run; 0 means a 10 minute default. A run that
	// exceeds it is abandoned and recorded as a failure — it must never block
	// the next run or, via the store, API traffic.
	Timeout time.Duration
}

// RunBackups snapshots the database immediately and then on every tick until
// ctx is canceled. Failures are recorded for /healthz and logged, never fatal.
func (s *Server) RunBackups(ctx context.Context, cfg BackupConfig) {
	every := cfg.Every
	if every <= 0 {
		every = defaultBackupEvery
	}
	// Recorded even when the scheduler is off: /healthz reports "no scheduler"
	// as a degraded state, and it can only know from here.
	s.mu.Lock()
	s.backupDir, s.backupEvery = cfg.Dir, every
	s.mu.Unlock()
	if cfg.Dir == "" {
		return
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	run := func() {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		path, err := s.store.BackupContext(runCtx, cfg.Dir)
		s.mu.Lock()
		status := backupStatus{At: time.Now()}
		if err != nil {
			// Keep the last successful snapshot's time visible in /healthz
			// alongside the error, so a failing scheduler is diagnosable.
			status = s.lastBackup
			status.Error = err.Error()
		}
		s.lastBackup = status
		s.mu.Unlock()
		if err != nil {
			log.Printf("backup failed: %v", err)
			return
		}
		log.Printf("backup written: %s", path)
		if cfg.Keep > 0 {
			if _, err := store.PruneBackups(cfg.Dir, cfg.Keep); err != nil {
				log.Printf("backup prune failed: %v", err)
			}
		}
	}
	// A service that restarts often must not fill the backup directory with
	// near-identical snapshots, so a recent one stands in for the startup run.
	if age, ok := store.NewestBackupAge(cfg.Dir); ok && age < every/4 {
		s.mu.Lock()
		s.lastBackup = backupStatus{At: time.Now().Add(-age)}
		s.mu.Unlock()
		log.Printf("backup skipped: newest snapshot is %s old", age.Round(time.Second))
		// The interval belongs to the snapshots, not to this process: waiting a
		// full interval after a restart would leave a gap of almost twice the
		// interval and trip a backup-age alarm that has nothing wrong to report.
		timer := time.NewTimer(firstBackupDelay(every, age))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			run()
		}
	} else {
		run()
	}
	ticker := time.NewTicker(every)
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

// firstBackupDelay is how long to wait for the first scheduled run when the
// startup run was skipped: what is left of the interval the existing snapshot
// has already spent. A snapshot older than the interval is due now.
func firstBackupDelay(every, age time.Duration) time.Duration {
	if age >= every {
		return 0
	}
	return every - age
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), requestKey, info)))
		name := info.actor
		if name == "" {
			name = "-"
		}
		log.Printf("%s %s %d %s actor=%s", r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond), name)
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

// jsonMuxErrors gives the mux's own 404 and 405 replies the JSON error body
// the rest of the API uses. The mux writes them itself, so there is no handler
// to change: they are caught on the way out, and only under the API paths. A
// browser hitting a bad UI URL still gets the plain page, and /mcp keeps
// whatever shape the protocol's own transport chose.
func jsonMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&jsonErrorWriter{ResponseWriter: w}, r)
	})
}

type jsonErrorWriter struct {
	http.ResponseWriter
	swallow bool
}

func (w *jsonErrorWriter) WriteHeader(status int) {
	// http.Error, which is what the mux uses for both replies, sets the plain
	// text content type before the status; anything else on the way out is a
	// real handler's response and is left alone.
	if (status != http.StatusNotFound && status != http.StatusMethodNotAllowed) ||
		!strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.swallow = true
	msg := "no route for this path"
	if status == http.StatusMethodNotAllowed {
		msg = "method not allowed for this path"
	}
	w.Header().Set("Content-Type", "application/json")
	w.ResponseWriter.WriteHeader(status)
	body, err := json.Marshal(errorBody{Error: msg, Code: codeNotFound})
	if err != nil {
		return
	}
	if _, err := w.ResponseWriter.Write(append(body, '\n')); err != nil {
		log.Printf("write response: %v", err)
	}
}

func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if w.swallow {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// The machine-readable half of an error body. Clients switch on these, so the
// set is closed: a new class needs a new name here and in the CLI's mapping.
const (
	codeNotFound           = "not_found"
	codeInvalidRef         = "invalid_ref"
	codeConflict           = "conflict"
	codeVersionConflict    = "version_conflict"
	codeDescriptionReplace = "description_replace"
	codeBusy               = "busy"
	codeValidation         = "validation"
	codeInternal           = "internal"
	codeUnauthorized       = "unauthorized"
	codeForbidden          = "forbidden"
)

// errAdminOnly is the one rule this package enforces on top of the store's own:
// replacing a description is an admin-token action. The append-only default
// already makes an accidental overwrite impossible; this puts the deliberate
// overwrite behind an identity a person holds, because the agent that erased a
// runbook is not the one who notices.
var errAdminOnly = errors.New("replacing a description needs an admin token; agents use append")

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

// writeValidation is the answer to a request this package rejected before the
// store ever saw it.
func writeValidation(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusBadRequest, codeValidation, msg)
}

// classify maps a store failure, or this package's own errAdminOnly, onto its
// HTTP status and machine code.
//
// The two specific conflicts are tested first because both also satisfy
// ErrConflict. Beyond the sentinels the store reports its own validation
// failures (an unknown enum value, an empty body, a malformed date) as plain
// errors, so the practical rule is: anything that is not a sentinel is the
// caller's fault and answers 400, except an error carrying a driver or
// filesystem failure, which is ours and answers 500 with the detail kept in
// the log rather than the body.
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, store.ErrVersionConflict):
		return http.StatusConflict, codeVersionConflict
	case errors.Is(err, store.ErrDescriptionReplace):
		return http.StatusConflict, codeDescriptionReplace
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, codeConflict
	case errors.Is(err, store.ErrInvalidRef):
		return http.StatusUnprocessableEntity, codeInvalidRef
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, codeNotFound
	case errors.Is(err, store.ErrBusy):
		return http.StatusServiceUnavailable, codeBusy
	case errors.Is(err, errAdminOnly):
		return http.StatusForbidden, codeForbidden
	case isInternal(err):
		return http.StatusInternalServerError, codeInternal
	default:
		return http.StatusBadRequest, codeValidation
	}
}

// isInternal reports whether an unclassified error came from the machine
// rather than the request: a SQLite fault, a damaged or too-new database, a
// lock another process holds, or the filesystem refusing to co-operate.
func isInternal(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return true
	}
	if errors.Is(err, store.ErrIntegrity) || errors.Is(err, store.ErrSchemaNewer) || errors.Is(err, store.ErrLocked) {
		return true
	}
	return errors.As(err, new(*fs.PathError)) ||
		errors.As(err, new(*os.LinkError)) ||
		errors.As(err, new(*os.SyscallError))
}

func writeStoreError(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	if status == http.StatusServiceUnavailable {
		// The CLI retries a busy database rather than reporting a failure.
		w.Header().Set("Retry-After", "1")
	}
	if status == http.StatusInternalServerError {
		log.Printf("%s %s: internal error: %v", r.Method, r.URL.Path, err)
		writeError(w, status, code, "internal error")
		return
	}
	writeError(w, status, code, err.Error())
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
