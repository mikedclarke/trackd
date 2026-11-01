package server

import (
	"fmt"
	"net/http"
	"strings"
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

// A private view stays out of the shared activity feed for everyone who could
// not read it directly, over REST and MCP alike, and hiding it never ends a
// page early.
func TestPrivateViewHiddenFromFeed(t *testing.T) {
	e := newTestEnv(t) // pm owns the view
	other, err := e.store.CreateToken("alex", "agent")
	if err != nil {
		t.Fatal(err)
	}
	admin := e.adminToken()
	otherMCP := mcpSessionAs(t, e.store, "sam", "agent")
	adminMCP := mcpSessionAs(t, e.store, "root", "admin")
	const marker = "pm-only-notes"
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "first"}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/views", map[string]any{
		"name": "Mine", "description": marker, "shared": false,
		"filter": map[string]any{"statuses": []string{"Todo"}},
	}, http.StatusCreated, nil)
	e.expect("POST", "/api/v1/issues", map[string]any{"title": "second"}, http.StatusCreated, nil)

	restSees := func(query string) bool {
		t.Helper()
		var feed eventListResponse
		e.expect("GET", "/api/v1/events"+query, nil, http.StatusOK, &feed)
		for _, ev := range feed.Events {
			if strings.Contains(string(ev.After), marker) {
				return true
			}
		}
		return false
	}
	for _, query := range []string{"", "?entity=view"} {
		if !restSees(query) {
			t.Errorf("owner: REST feed%s lacks the private view", query)
		}
		e.withToken(other, func() {
			if restSees(query) {
				t.Errorf("another token: REST feed%s carries the private view", query)
			}
		})
		e.withToken(admin, func() {
			if !restSees(query) {
				t.Errorf("admin: REST feed%s lacks the private view", query)
			}
		})
	}
	for _, args := range []map[string]any{nil, {"entity": "view"}} {
		var act mcpActivityOut
		callTool(t, otherMCP, "list_activity", args, &act)
		if strings.Contains(fmt.Sprint(act.Events), marker) {
			t.Errorf("another token: list_activity %v carries the private view", args)
		}
		callTool(t, adminMCP, "list_activity", args, &act)
		if !strings.Contains(fmt.Sprint(act.Events), marker) {
			t.Errorf("admin: list_activity %v lacks the private view", args)
		}
	}

	// One event a page: the other token's pages stay full, each with a
	// cursor, until the last, and together they are its unpaged feed.
	e.withToken(other, func() {
		var whole eventListResponse
		e.expect("GET", "/api/v1/events", nil, http.StatusOK, &whole)
		var paged []int64
		path := "/api/v1/events?limit=1"
		for range 20 {
			var page eventListResponse
			e.expect("GET", path, nil, http.StatusOK, &page)
			if len(page.Events) == 0 {
				break
			}
			if page.NextAfterID == nil {
				t.Fatalf("a full page after %v carried no cursor", paged)
			}
			paged = append(paged, page.Events[0].ID)
			path = fmt.Sprintf("/api/v1/events?limit=1&after_id=%d", *page.NextAfterID)
		}
		if len(paged) != len(whole.Events) {
			t.Fatalf("paged %d events, unpaged %d", len(paged), len(whole.Events))
		}
		for i, ev := range whole.Events {
			if ev.Entity == "view" || paged[i] != ev.ID {
				t.Errorf("event %d: paged id %d, unpaged %+v", i, paged[i], ev)
			}
		}
	})
}
