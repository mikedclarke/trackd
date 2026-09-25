package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mikedclarke/trackd/internal/store"
)

// viewPageLimit is how many rows one page of a view shows; the next page is
// a plain link, so a long queue still works without script.
const viewPageLimit = 100

var (
	viewTemplate     = parseUITemplate("view.html")
	viewFormTemplate = parseUITemplate("view_form.html")
)

// uiTabs are the views the signed-in token may open, shown as tabs beside the
// board on every list page.
func (s *Server) uiTabs(r *http.Request) ([]store.View, error) {
	actor, role := uiIdentity(r)
	return s.store.ListViews(actor, role == roleAdmin, false)
}

// uiNotice is the one-line outcome of a form post, carried back to the page
// in the query string so the redirect can say what happened.
type uiNotice struct {
	Kind string // "saved", "conflict", "error"
	Key  string
	Text string
}

func noticeFrom(q url.Values) *uiNotice {
	kind := q.Get("notice")
	if kind == "" {
		return nil
	}
	n := &uiNotice{Kind: kind, Key: q.Get("key"), Text: q.Get("msg")}
	switch kind {
	case "saved":
		n.Text = n.Key + " updated."
	case "conflict":
		n.Text = n.Key + " changed since this page was loaded, so the change was not applied" +
			"; a reply typed with it was still saved. Reload and try again."
	}
	return n
}

// withNotice appends a notice to a page URL, replacing any notice already on it.
func withNotice(back string, n uiNotice) string {
	u, err := url.Parse(back)
	if err != nil {
		u = &url.URL{Path: "/"}
	}
	q := u.Query()
	q.Set("notice", n.Kind)
	if n.Key != "" {
		q.Set("key", n.Key)
	} else {
		q.Del("key")
	}
	if n.Kind == "error" {
		q.Set("msg", n.Text)
	} else {
		q.Del("msg")
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

// safeBack accepts a same-site path to return to after a form post; anything
// else, including a scheme or a protocol-relative host, falls back to the
// board.
func safeBack(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.ContainsAny(raw, "\\\r\n") {
		return raw
	}
	return "/"
}

// viewRow is one issue on a view page with what was last said on it.
type viewRow struct {
	Issue   store.Issue
	Latest  *store.Comment
	Actions []viewRowAction
}

// viewRowAction is a quick action as the row's form posts it.
type viewRowAction struct {
	Name string
}

// quickActionByName finds a view's quick action by its (unique) name.
func quickActionByName(view *store.View, name string) (store.QuickAction, bool) {
	if view == nil {
		return store.QuickAction{}, false
	}
	for _, q := range view.QuickActions {
		if q.Name == name {
			return q, true
		}
	}
	return store.QuickAction{}, false
}

func (s *Server) uiView(w http.ResponseWriter, r *http.Request) {
	actor, role := uiIdentity(r)
	view, err := s.visibleView(r.PathValue("name"), actor, role)
	if err != nil || view.ArchivedAt != "" {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	offset, err := intParam(r.URL.Query(), "offset")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filter, err := view.Filter.Apply(store.IssueFilter{Limit: viewPageLimit, Offset: offset}, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	issues, err := s.store.ListIssues(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ids := make([]int64, len(issues))
	for i := range issues {
		ids[i] = issues[i].ID
	}
	latest, err := s.store.LatestComments(ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var actions []viewRowAction
	for _, q := range view.QuickActions {
		actions = append(actions, viewRowAction{Name: q.Name})
	}
	rows := make([]viewRow, len(issues))
	for i, issue := range issues {
		rows[i] = viewRow{Issue: issue, Actions: actions}
		if c, ok := latest[issue.ID]; ok {
			c := c
			rows[i].Latest = &c
		}
	}
	statuses, err := s.store.ListStatuses()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	labels, err := s.store.ListLabels()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tabs, err := s.uiTabs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	self := "/ui/view/" + url.PathEscape(view.Name)
	next := ""
	if len(issues) == viewPageLimit {
		next = self + "?offset=" + strconv.Itoa(offset+viewPageLimit)
	}
	prev := ""
	if offset > 0 {
		prev = self
		if offset > viewPageLimit {
			prev += "?offset=" + strconv.Itoa(offset-viewPageLimit)
		}
	}
	s.render(w, viewTemplate, map[string]any{
		"Title":    view.Name,
		"View":     view,
		"Views":    tabs,
		"Current":  view.Name,
		"Rows":     rows,
		"Count":    len(issues),
		"Offset":   offset,
		"Next":     next,
		"Prev":     prev,
		"Statuses": statuses,
		"Labels":   labels,
		"Actor":    actor,
		"CanEdit":  canEditView(view, actor, role),
		"Back":     self,
		"Notice":   noticeFrom(r.URL.Query()),
		"Filter":   describeViewFilter(view.Filter),
	})
}

// describeViewFilter renders a filter as short "field: value" pairs for the
// view page header.
func describeViewFilter(f store.ViewFilter) []string {
	var parts []string
	pair := func(name string, values ...string) {
		if len(values) > 0 && strings.Join(values, "") != "" {
			parts = append(parts, name+": "+strings.Join(values, ", "))
		}
	}
	pair("status", f.Statuses...)
	pair("type", f.StatusTypes...)
	pair("project", f.Project)
	pair("label", f.Labels...)
	pair("not", f.ExcludeLabels...)
	pair("assignee", f.Assignee)
	pair("milestone", f.Milestone)
	var prios []string
	for _, p := range f.Priorities {
		prios = append(prios, "P"+strconv.Itoa(p))
	}
	pair("priority", prios...)
	pair("updated within", f.UpdatedWithin)
	pair("created by", f.CreatedBy)
	pair("search", f.Query)
	pair("order", f.OrderBy)
	return parts
}

// uiIssueAction is the one write path behind every row form and the issue
// page's own action box: an optional reply, then a patch made of the fields
// the form set plus a quick action if one was pressed. The patch carries the
// version the page was rendered with, so a change that lost a race answers
// with a conflict notice rather than overwriting; the reply is posted first
// because a reply is never wrong, and it survives a conflict.
func (s *Server) uiIssueAction(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
		http.Error(w, "cross-origin form post refused", http.StatusForbidden)
		return
	}
	key := r.PathValue("key")
	back := safeBack(r.PostFormValue("back"))
	if back == "/" {
		back = "/ui/issue/" + url.PathEscape(key)
	}
	actor, role := uiIdentity(r)
	fail := func(msg string) {
		http.Redirect(w, r, withNotice(back, uiNotice{Kind: "error", Key: key, Text: msg}), http.StatusSeeOther)
	}
	body := strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n")
	if strings.TrimSpace(body) != "" {
		if _, _, err := s.store.AddComment(key, store.CommentInput{Body: body}, actor); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			fail(err.Error())
			return
		}
	}
	patch, err := actionPatch(r.PostForm)
	if err != nil {
		fail(err.Error())
		return
	}
	if do := r.PostFormValue("do"); strings.HasPrefix(do, "quick:") {
		// The button posts the action's name, not its position, so a view
		// edited between the page render and the tap applies the action the
		// person read or nothing at all.
		view, err := s.visibleView(r.PostFormValue("view"), actor, role)
		q, ok := quickActionByName(view, strings.TrimPrefix(do, "quick:"))
		if err != nil || !ok {
			fail("that quick action no longer exists; reload the view")
			return
		}
		patch = mergePatch(patch, q.Patch())
	}
	if patchEmpty(patch) {
		if strings.TrimSpace(body) == "" {
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, withNotice(back, uiNotice{Kind: "saved", Key: key}), http.StatusSeeOther)
		return
	}
	if _, err := s.store.UpdateIssue(key, patch, actor); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.NotFound(w, r)
		case errors.Is(err, store.ErrVersionConflict):
			http.Redirect(w, r, withNotice(back, uiNotice{Kind: "conflict", Key: key}), http.StatusSeeOther)
		default:
			fail(err.Error())
		}
		return
	}
	http.Redirect(w, r, withNotice(back, uiNotice{Kind: "saved", Key: key}), http.StatusSeeOther)
}

// actionPatch reads the optional status, priority and label fields of a row
// or issue form. Every field defaults to "leave alone"; expected_version is
// required, because a form that does not know what it is changing from must
// not change anything.
func actionPatch(form url.Values) (store.IssuePatch, error) {
	var p store.IssuePatch
	raw := form.Get("expected_version")
	if raw == "" {
		return p, errors.New("the form did not carry the issue's version; reload the page")
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return p, errors.New("the form carried a bad version; reload the page")
	}
	p.ExpectedVersion = &version
	if status := form.Get("status"); status != "" {
		p.Status = &status
	}
	if raw := form.Get("priority"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 4 {
			return p, fmt.Errorf("priority %q is not 0-4", raw)
		}
		p.Priority = &n
	}
	if label := form.Get("add_label"); label != "" {
		p.AddLabels = append(p.AddLabels, label)
	}
	if label := form.Get("remove_label"); label != "" {
		p.RemoveLabels = append(p.RemoveLabels, label)
	}
	return p, nil
}

// mergePatch lays a quick action's patch over the form's: the action's own
// fields win, and label edits from both are kept.
func mergePatch(base, over store.IssuePatch) store.IssuePatch {
	if over.Status != nil {
		base.Status = over.Status
	}
	if over.Priority != nil {
		base.Priority = over.Priority
	}
	base.AddLabels = append(base.AddLabels, over.AddLabels...)
	base.RemoveLabels = append(base.RemoveLabels, over.RemoveLabels...)
	return base
}

func patchEmpty(p store.IssuePatch) bool {
	return p.Status == nil && p.Priority == nil && len(p.AddLabels) == 0 && len(p.RemoveLabels) == 0
}

// viewForm is what the new-view and edit-view pages render: the values (from
// the stored view, or from a submission that failed) and the choices.
type viewForm struct {
	Name         string
	Description  string
	Filter       store.ViewFilter
	Quick        []store.QuickAction
	Shared       bool
	Editing      bool
	Original     string // the stored name, for the edit page's action and links
	Error        string
	Statuses     []store.Status
	Labels       []store.Label
	Projects     []store.Project
	Assignees    []string
	StatusTypes  []string
	OrderOptions []string
}

func (s *Server) newViewForm(r *http.Request) (*viewForm, error) {
	f := &viewForm{Shared: true, StatusTypes: []string{"triage", "backlog", "unstarted", "started", "completed", "canceled"},
		OrderOptions: []string{"updated", "created", "priority"}}
	var err error
	if f.Statuses, err = s.store.ListStatuses(); err != nil {
		return nil, err
	}
	if f.Labels, err = s.store.ListLabels(); err != nil {
		return nil, err
	}
	if f.Projects, err = s.store.ListProjects(false); err != nil {
		return nil, err
	}
	if f.Assignees, err = s.store.ListAssignees(); err != nil {
		return nil, err
	}
	// The form always shows the full set of quick action rows so a person can
	// add one without a script adding the row.
	f.Quick = make([]store.QuickAction, store.MaxQuickActions)
	return f, nil
}

func (s *Server) renderViewForm(w http.ResponseWriter, r *http.Request, f *viewForm, status int) {
	tabs, err := s.uiTabs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	title := "new view"
	if f.Editing {
		title = "edit " + f.Original
	}
	w.WriteHeader(status)
	s.render(w, viewFormTemplate, map[string]any{
		"Title":   title,
		"Views":   tabs,
		"Current": f.Original,
		"Form":    f,
	})
}

func (s *Server) uiViewNew(w http.ResponseWriter, r *http.Request) {
	f, err := s.newViewForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderViewForm(w, r, f, http.StatusOK)
}

func (s *Server) uiViewEdit(w http.ResponseWriter, r *http.Request) {
	actor, role := uiIdentity(r)
	view, err := s.visibleView(r.PathValue("name"), actor, role)
	if err != nil || view.ArchivedAt != "" {
		http.NotFound(w, r)
		return
	}
	if !canEditView(view, actor, role) {
		http.Error(w, errViewOwner.Error(), http.StatusForbidden)
		return
	}
	f, err := s.newViewForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.Editing, f.Original = true, view.Name
	f.Name, f.Description, f.Filter, f.Shared = view.Name, view.Description, view.Filter, view.Shared
	copy(f.Quick, view.QuickActions)
	s.renderViewForm(w, r, f, http.StatusOK)
}

// readViewForm fills a form from a submission. Multi-value selects arrive as
// repeated fields; an empty option is "not set".
func readViewForm(f *viewForm, form url.Values) error {
	f.Name = strings.TrimSpace(form.Get("name"))
	f.Description = strings.TrimSpace(form.Get("description"))
	f.Shared = form.Get("shared") != ""
	f.Filter = store.ViewFilter{
		Statuses:      nonEmpty(form["status"]),
		StatusTypes:   nonEmpty(form["status_type"]),
		Project:       form.Get("project"),
		Labels:        nonEmpty(form["label"]),
		ExcludeLabels: nonEmpty(form["exclude_label"]),
		Assignee:      strings.TrimSpace(form.Get("assignee")),
		Milestone:     strings.TrimSpace(form.Get("milestone")),
		UpdatedWithin: strings.TrimSpace(form.Get("updated_within")),
		CreatedBy:     strings.TrimSpace(form.Get("created_by")),
		Query:         strings.TrimSpace(form.Get("query")),
		OrderBy:       form.Get("order_by"),
	}
	for _, raw := range form["priority"] {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("priority %q is not a number", raw)
		}
		f.Filter.Priorities = append(f.Filter.Priorities, n)
	}
	for i := range f.Quick {
		suffix := strconv.Itoa(i)
		q := store.QuickAction{
			Name:   strings.TrimSpace(form.Get("quick_name_" + suffix)),
			Status: form.Get("quick_status_" + suffix),
		}
		if raw := form.Get("quick_priority_" + suffix); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("quick action priority %q is not a number", raw)
			}
			q.Priority = &n
		}
		q.AddLabels = nonEmpty(form["quick_add_"+suffix])
		q.RemoveLabels = nonEmpty(form["quick_remove_"+suffix])
		f.Quick[i] = q
	}
	return nil
}

// quickActions drops the form's blank rows: a row with no name is unused.
func (f *viewForm) quickActions() []store.QuickAction {
	out := []store.QuickAction{}
	for _, q := range f.Quick {
		if q.Name != "" || q.Status != "" || q.Priority != nil || len(q.AddLabels) > 0 || len(q.RemoveLabels) > 0 {
			out = append(out, q)
		}
	}
	return out
}

func nonEmpty(values []string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) uiViewCreate(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
		http.Error(w, "cross-origin form post refused", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := s.newViewForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := readViewForm(f, r.PostForm); err != nil {
		f.Error = err.Error()
		s.renderViewForm(w, r, f, http.StatusBadRequest)
		return
	}
	actor, _ := uiIdentity(r)
	shared := f.Shared
	view, err := s.store.CreateView(store.ViewInput{
		Name: f.Name, Description: f.Description, Filter: f.Filter,
		QuickActions: f.quickActions(), Shared: &shared,
	}, actor)
	if err != nil {
		f.Error = err.Error()
		s.renderViewForm(w, r, f, formErrorStatus(err))
		return
	}
	http.Redirect(w, r, "/ui/view/"+url.PathEscape(view.Name), http.StatusSeeOther)
}

func (s *Server) uiViewUpdate(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
		http.Error(w, "cross-origin form post refused", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor, role := uiIdentity(r)
	view, err := s.visibleView(r.PathValue("name"), actor, role)
	if err != nil || view.ArchivedAt != "" {
		http.NotFound(w, r)
		return
	}
	if !canEditView(view, actor, role) {
		http.Error(w, errViewOwner.Error(), http.StatusForbidden)
		return
	}
	if r.PostFormValue("do") == "archive" {
		yes := true
		if _, err := s.store.UpdateView(view.Name, store.ViewPatch{Archived: &yes}, actor); err != nil {
			http.Error(w, err.Error(), formErrorStatus(err))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	f, err := s.newViewForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.Editing, f.Original = true, view.Name
	if err := readViewForm(f, r.PostForm); err != nil {
		f.Error = err.Error()
		s.renderViewForm(w, r, f, http.StatusBadRequest)
		return
	}
	quick := f.quickActions()
	updated, err := s.store.UpdateView(view.Name, store.ViewPatch{
		Name: &f.Name, Description: &f.Description, Filter: &f.Filter,
		QuickActions: &quick, Shared: &f.Shared,
	}, actor)
	if err != nil {
		f.Error = err.Error()
		s.renderViewForm(w, r, f, formErrorStatus(err))
		return
	}
	http.Redirect(w, r, "/ui/view/"+url.PathEscape(updated.Name), http.StatusSeeOther)
}

// formErrorStatus maps a store failure on a form post to the status the page
// re-renders with: the caller's mistake reads as 400-class, ours as 500.
func formErrorStatus(err error) int {
	status, _ := classify(err)
	if status >= 500 {
		return http.StatusInternalServerError
	}
	return status
}
