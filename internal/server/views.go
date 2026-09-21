package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mikedclarke/trackd/internal/store"
)

// errViewOwner is the second role rule this package enforces: a view is
// changed by the token that made it or by an admin. Anyone may read a shared
// view and apply it; a private view is invisible to everyone else.
var errViewOwner = errors.New("only the view's owner or an admin token can change it")

// canEditView reports whether actor, holding role, may change a view.
func canEditView(v *store.View, actor, role string) bool {
	return role == roleAdmin || (actor != "" && v.Owner == actor)
}

// canSeeView reports whether a view is visible to actor: shared views are
// visible to everyone, private ones to their owner and to admins.
func canSeeView(v *store.View, actor, role string) bool {
	return v.Shared || canEditView(v, actor, role)
}

// visibleView loads a view by name as actor sees it. A private view of
// someone else's answers not found rather than forbidden, so a name is not
// confirmed to a token that may not see it.
func (s *Server) visibleView(name, actor, role string) (*store.View, error) {
	view, err := s.store.GetView(name)
	if err != nil {
		return nil, err
	}
	if !canSeeView(view, actor, role) {
		return nil, store.ErrNotFound
	}
	return view, nil
}

// applyView lays a named view under a request's filter, so the request can
// narrow or re-sort the view without editing it.
func (s *Server) applyView(name, actor, role string, f store.IssueFilter) (store.IssueFilter, error) {
	if name == "" {
		return f, nil
	}
	view, err := s.visibleView(name, actor, role)
	if err != nil {
		return f, err
	}
	if view.ArchivedAt != "" {
		// An archived view can still be read, but its name is free for a new
		// view, so applying it would be applying whatever the name last meant.
		return f, fmt.Errorf("view %q is archived: %w", view.Name, store.ErrNotFound)
	}
	return view.Filter.Apply(f, time.Now())
}

type viewListResponse struct {
	Views []store.View `json:"views"`
}

func (s *Server) handleListViews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := checkParams(q, []string{"archived"}); err != nil {
		writeValidation(w, err.Error())
		return
	}
	views, err := s.store.ListViews(actor(r, ""), tokenRole(r) == roleAdmin, q.Get("archived") == "true")
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewListResponse{Views: views})
}

type viewCreateReq struct {
	Name         string              `json:"name"`
	Description  string              `json:"description"`
	Filter       store.ViewFilter    `json:"filter"`
	QuickActions []store.QuickAction `json:"quick_actions"`
	Shared       *bool               `json:"shared"`
	Actor        string              `json:"actor"`
}

func (s *Server) handleCreateView(w http.ResponseWriter, r *http.Request) {
	var req viewCreateReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	view, err := s.store.CreateView(store.ViewInput{
		Name:         req.Name,
		Description:  req.Description,
		Filter:       req.Filter,
		QuickActions: req.QuickActions,
		Shared:       req.Shared,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) handleGetView(w http.ResponseWriter, r *http.Request) {
	view, err := s.visibleView(r.PathValue("name"), actor(r, ""), tokenRole(r))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

type viewPatchReq struct {
	Name         *string              `json:"name"`
	Description  *string              `json:"description"`
	Filter       *store.ViewFilter    `json:"filter"`
	QuickActions *[]store.QuickAction `json:"quick_actions"`
	Shared       *bool                `json:"shared"`
	Archived     *bool                `json:"archived"`
	Actor        string               `json:"actor"`
}

func (p viewPatchReq) empty() bool {
	return p.Name == nil && p.Description == nil && p.Filter == nil && p.QuickActions == nil &&
		p.Shared == nil && p.Archived == nil
}

func (s *Server) handlePatchView(w http.ResponseWriter, r *http.Request) {
	var req viewPatchReq
	if err := decodeBody(w, r, &req); err != nil {
		writeValidation(w, err.Error())
		return
	}
	if req.empty() {
		writeValidation(w, "empty patch: no fields to update")
		return
	}
	act, role := actor(r, ""), tokenRole(r)
	view, err := s.visibleView(r.PathValue("name"), act, role)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !canEditView(view, act, role) {
		writeStoreError(w, r, errViewOwner)
		return
	}
	updated, err := s.store.UpdateView(view.Name, store.ViewPatch{
		Name:         req.Name,
		Description:  req.Description,
		Filter:       req.Filter,
		QuickActions: req.QuickActions,
		Shared:       req.Shared,
		Archived:     req.Archived,
	}, actor(r, req.Actor))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
