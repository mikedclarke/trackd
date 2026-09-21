package server

import (
	"net/http"
	"testing"

	"github.com/mikedclarke/trackd/internal/store"
)

func TestViewsAPI(t *testing.T) {
	e := newTestEnv(t)
	e.label("waiting", "ready")
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "parked", "status": "Todo", "labels": []string{"waiting"}, "priority": 2}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "free", "status": "Todo"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "done", "status": "Done", "labels": []string{"waiting"}}, http.StatusCreated, nil)

	var view store.View
	e.expect("POST", "/api/v1/views", map[string]any{
		"name":        "Waiting on me",
		"description": "parked on a person",
		"filter":      map[string]any{"statuses": []string{"Todo", "In Progress"}, "labels": []string{"waiting"}, "order_by": "created"},
		"quick_actions": []map[string]any{
			{"name": "Answered", "remove_labels": []string{"waiting"}},
		},
	}, http.StatusCreated, &view)
	if view.Owner != "pm" || !view.Shared || len(view.QuickActions) != 1 {
		t.Fatalf("view = %+v", view)
	}

	// The view applies through the issue list, and explicit filters layer on
	// top of it.
	got := e.issues("?view=Waiting%20on%20me")
	if len(got.Issues) != 1 || got.Issues[0].Title != "parked" {
		t.Fatalf("view issues = %+v", got.Issues)
	}
	got = e.issues("?view=waiting%20on%20me&status=Done")
	if len(got.Issues) != 1 || got.Issues[0].Title != "done" {
		t.Fatalf("view with override = %+v", got.Issues)
	}
	got = e.issues("?priority=2&priority=1")
	if len(got.Issues) != 1 || got.Issues[0].Title != "parked" {
		t.Fatalf("priority filter = %+v", got.Issues)
	}
	got = e.issues("?created_by=pm")
	if len(got.Issues) != 3 {
		t.Fatalf("created_by filter = %d issues", len(got.Issues))
	}
	e.expectError("GET", "/api/v1/issues?view=nope", nil, http.StatusNotFound, codeNotFound)
	e.expectError("GET", "/api/v1/issues?priority=x", nil, http.StatusBadRequest, codeValidation)
	e.expectError("GET", "/api/v1/issues?priority=9", nil, http.StatusUnprocessableEntity, codeInvalidRef)

	// Read, list, patch.
	var fetched store.View
	e.expect("GET", "/api/v1/views/Waiting%20on%20me", nil, http.StatusOK, &fetched)
	if fetched.ID != view.ID {
		t.Fatalf("get = %+v", fetched)
	}
	var list viewListResponse
	e.expect("GET", "/api/v1/views", nil, http.StatusOK, &list)
	if len(list.Views) != 1 {
		t.Fatalf("list = %+v", list)
	}
	e.expectError("PATCH", "/api/v1/views/Waiting%20on%20me", map[string]any{}, http.StatusBadRequest, codeValidation)
	e.expectError("PATCH", "/api/v1/views/Waiting%20on%20me", map[string]any{"filter": map[string]any{}}, http.StatusUnprocessableEntity, codeInvalidRef)
	e.expectError("POST", "/api/v1/views", map[string]any{"name": "waiting on ME", "filter": map[string]any{"statuses": []string{"Todo"}}}, http.StatusConflict, codeConflict)
	e.expectError("POST", "/api/v1/views", map[string]any{"name": "x", "filter": map[string]any{"labels": []string{"nope"}}}, http.StatusUnprocessableEntity, codeInvalidRef)
	e.expectError("POST", "/api/v1/views", map[string]any{"name": "x", "filter": map[string]any{"statuses": []string{"Todo"}}, "bogus": 1}, http.StatusBadRequest, codeValidation)

	var patched store.View
	e.expect("PATCH", "/api/v1/views/Waiting%20on%20me", map[string]any{"description": "changed", "shared": false}, http.StatusOK, &patched)
	if patched.Description != "changed" || patched.Shared {
		t.Fatalf("patched = %+v", patched)
	}

	// Another agent cannot see a private view, nor change a shared one it
	// does not own; an admin can do both.
	other, err := e.store.CreateToken("seo", "agent")
	if err != nil {
		t.Fatal(err)
	}
	e.withToken(other, func() {
		e.expectError("GET", "/api/v1/views/Waiting%20on%20me", nil, http.StatusNotFound, codeNotFound)
		e.expectError("GET", "/api/v1/issues?view=Waiting%20on%20me", nil, http.StatusNotFound, codeNotFound)
		e.expect("GET", "/api/v1/views", nil, http.StatusOK, &list)
		if len(list.Views) != 0 {
			t.Errorf("private view listed to another token: %+v", list.Views)
		}
	})
	e.expect("PATCH", "/api/v1/views/Waiting%20on%20me", map[string]any{"shared": true}, http.StatusOK, nil)
	e.withToken(other, func() {
		e.expect("GET", "/api/v1/views/Waiting%20on%20me", nil, http.StatusOK, nil)
		e.expectError("PATCH", "/api/v1/views/Waiting%20on%20me", map[string]any{"description": "mine now"}, http.StatusForbidden, codeForbidden)
	})
	e.withToken(e.adminToken(), func() {
		e.expect("PATCH", "/api/v1/views/Waiting%20on%20me", map[string]any{"archived": true}, http.StatusOK, &patched)
	})
	if patched.ArchivedAt == "" {
		t.Fatalf("archived = %+v", patched)
	}
	e.expect("GET", "/api/v1/views", nil, http.StatusOK, &list)
	if len(list.Views) != 0 {
		t.Errorf("archived view still listed: %+v", list.Views)
	}
	e.expect("GET", "/api/v1/views?archived=true", nil, http.StatusOK, &list)
	if len(list.Views) != 1 {
		t.Errorf("archived view missing from archived listing: %+v", list.Views)
	}
	// An archived view is not applied: its name has been freed.
	e.expectError("GET", "/api/v1/issues?view=Waiting%20on%20me", nil, http.StatusNotFound, codeNotFound)
	var feed eventListResponse
	e.expect("GET", "/api/v1/events?entity=view", nil, http.StatusOK, &feed)
	if len(feed.Events) != 4 || feed.Events[3].Action != "view.archived" || feed.Events[3].EntityKey != "Waiting on me" {
		t.Errorf("view events = %+v", feed.Events)
	}
}
