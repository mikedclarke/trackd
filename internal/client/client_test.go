package client

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mikedclarke/trackd/internal/server"
	"github.com/mikedclarke/trackd/internal/store"
)

func TestClientRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	token, err := st.CreateToken("pm", "agent")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(st, "test").Handler())
	t.Cleanup(ts.Close)

	c := New(ts.URL+"/", token) // trailing slash must be tolerated
	var issue store.Issue
	if err := c.Do("POST", "/api/v1/issues", nil, map[string]any{"title": "via client"}, &issue); err != nil {
		t.Fatal(err)
	}
	if issue.Key != "TSK-1" {
		t.Fatalf("issue = %+v", issue)
	}

	var got store.Issue
	if err := c.Do("GET", "/api/v1/issues/TSK-1", nil, nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "via client" {
		t.Errorf("get = %+v", got)
	}

	err = c.Do("GET", "/api/v1/issues/TSK-99", nil, nil, &got)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("missing issue error = %v, want *APIError 404", err)
	}

	bad := New(ts.URL, "td_wrong")
	err = bad.Do("GET", "/api/v1/issues", nil, nil, nil)
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Errorf("bad token error = %v, want *APIError 401", err)
	}
}
