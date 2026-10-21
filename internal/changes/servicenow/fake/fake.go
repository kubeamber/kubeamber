// Package fake is an in-memory ServiceNow instance with just enough of the
// Change Management and Table APIs for KubeAmber's bridge: creating change
// requests, reading them and their approvals, and updating them. It also has
// admin endpoints under /fake/ to approve or reject a change the way a CAB
// would. It is for tests and demos, not a ServiceNow emulator.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Server is a fake ServiceNow instance.
type Server struct {
	// Basic auth credentials and OAuth client credentials it accepts.
	Username, Password     string
	ClientID, ClientSecret string

	mu      sync.Mutex
	changes map[string]*Change // by sys_id
	tokens  map[string]bool
	seq     int
	// Requests logs "METHOD path" for every authenticated API call.
	Requests []string
}

// Change is a stored change request.
type Change struct {
	Fields    map[string]string `json:"fields"`
	WorkNotes []string          `json:"workNotes"`
	Approvals []Approval        `json:"approvals"`
}

// Approval is a sysapproval_approver record.
type Approval struct {
	User  string `json:"user"`
	State string `json:"state"`
}

// New returns a server that accepts the given basic-auth user and OAuth client.
func New(username, password, clientID, clientSecret string) *Server {
	return &Server{
		Username: username, Password: password, ClientID: clientID, ClientSecret: clientSecret,
		changes: map[string]*Change{}, tokens: map[string]bool{},
	}
}

// Handler serves the fake instance.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth_token.do", s.token)
	mux.HandleFunc("POST /api/sn_chg_rest/change/{type}", s.auth(s.create))
	mux.HandleFunc("GET /api/now/table/change_request", s.auth(s.query))
	mux.HandleFunc("GET /api/now/table/change_request/{id}", s.auth(s.get))
	mux.HandleFunc("PATCH /api/now/table/change_request/{id}", s.auth(s.patch))
	mux.HandleFunc("GET /api/now/table/sysapproval_approver", s.auth(s.approvals))
	mux.HandleFunc("POST /fake/changes/{number}/approve", s.decide("approved"))
	mux.HandleFunc("POST /fake/changes/{number}/reject", s.decide("rejected"))
	mux.HandleFunc("GET /fake/changes/{number}", s.show)
	return mux
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" ||
		r.Form.Get("client_id") != s.ClientID || r.Form.Get("client_secret") != s.ClientSecret || s.ClientID == "" {
		writeErr(w, http.StatusUnauthorized, "access_denied")
		return
	}
	s.mu.Lock()
	s.seq++
	tok := fmt.Sprintf("token-%d", s.seq)
	s.tokens[tok] = true
	s.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 1799})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok := false
		if u, p, basic := r.BasicAuth(); basic {
			ok = s.Username != "" && u == s.Username && p == s.Password
		} else if tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); bearer {
			s.mu.Lock()
			ok = s.tokens[tok]
			s.mu.Unlock()
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "User Not Authenticated")
			return
		}
		s.mu.Lock()
		s.Requests = append(s.Requests, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		next(w, r)
	}
}

// create answers like the Change Management API: every field as
// {"value", "display_value"}.
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("%032x", s.seq)
	c := &Change{Fields: map[string]string{}}
	for k, v := range body {
		c.Fields[k] = v
	}
	c.Fields["sys_id"] = id
	c.Fields["number"] = fmt.Sprintf("CHG%07d", 30000+s.seq)
	c.Fields["type"] = r.PathValue("type")
	c.Fields["state"] = "-5"
	c.Fields["approval"] = "requested"
	s.changes[id] = c
	out := map[string]any{}
	for k, v := range c.Fields {
		out[k] = map[string]string{"value": v, "display_value": v}
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	writeJSON(w, map[string]any{"result": out})
}

// query supports sysparm_query=correlation_id=<value>.
func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	want, _ := strings.CutPrefix(r.URL.Query().Get("sysparm_query"), "correlation_id=")
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]string
	for _, c := range s.sorted() {
		if c.Fields["correlation_id"] == want {
			out = append(out, pick(c.Fields, r.URL.Query().Get("sysparm_fields")))
		}
	}
	writeJSON(w, map[string]any{"result": orEmpty(out)})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.changes[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, "No Record found")
		return
	}
	writeJSON(w, map[string]any{"result": pick(c.Fields, r.URL.Query().Get("sysparm_fields"))})
}

func (s *Server) patch(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.changes[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, "No Record found")
		return
	}
	for k, v := range body {
		if k == "work_notes" {
			c.WorkNotes = append(c.WorkNotes, v)
			continue
		}
		c.Fields[k] = v
	}
	writeJSON(w, map[string]any{"result": c.Fields})
}

// approvals supports sysparm_query=sysapproval=<sys_id>^state=<state> and
// returns the dot-walked approver.user_name field.
func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	var id, state string
	for _, part := range strings.Split(r.URL.Query().Get("sysparm_query"), "^") {
		if v, ok := strings.CutPrefix(part, "sysapproval="); ok {
			id = v
		}
		if v, ok := strings.CutPrefix(part, "state="); ok {
			state = v
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]string
	if c, ok := s.changes[id]; ok {
		for _, a := range c.Approvals {
			if state == "" || a.State == state {
				out = append(out, map[string]string{"approver.user_name": a.User})
			}
		}
	}
	writeJSON(w, map[string]any{"result": orEmpty(out)})
}

// decide approves or rejects a change as user ?by=, the way a CAB would:
// approval moves it to Scheduled; rejection sets approval=rejected.
func (s *Server) decide(state string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		by := r.URL.Query().Get("by")
		if by == "" {
			by = "cab.approver"
		}
		if err := s.Decide(r.PathValue("number"), by, state); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		s.show(w, r)
	}
}

// Decide records an approval or rejection on the change with this number.
func (s *Server) Decide(number, user, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byNumber(number)
	if c == nil {
		return fmt.Errorf("no change %s", number)
	}
	c.Approvals = append(c.Approvals, Approval{User: user, State: state})
	c.Fields["approval"] = state
	if state == "approved" {
		c.Fields["state"] = "-2" // Scheduled
	}
	return nil
}

// Get returns a copy of the change with this number, or nil.
func (s *Server) Get(number string) *Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.byNumber(number)
	if c == nil {
		return nil
	}
	cp := &Change{Fields: map[string]string{}, WorkNotes: append([]string(nil), c.WorkNotes...), Approvals: append([]Approval(nil), c.Approvals...)}
	for k, v := range c.Fields {
		cp.Fields[k] = v
	}
	return cp
}

// Count returns how many change requests exist.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.changes)
}

func (s *Server) show(w http.ResponseWriter, r *http.Request) {
	c := s.Get(r.PathValue("number"))
	if c == nil {
		writeErr(w, http.StatusNotFound, "no such change")
		return
	}
	writeJSON(w, c)
}

func (s *Server) byNumber(number string) *Change {
	for _, c := range s.changes {
		if c.Fields["number"] == number {
			return c
		}
	}
	return nil
}

func (s *Server) sorted() []*Change {
	out := make([]*Change, 0, len(s.changes))
	for _, c := range s.changes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fields["number"] < out[j].Fields["number"] })
	return out
}

func pick(fields map[string]string, list string) map[string]string {
	if list == "" {
		return fields
	}
	out := map[string]string{}
	for _, f := range strings.Split(list, ",") {
		out[f] = fields[f]
	}
	return out
}

func orEmpty(rows []map[string]string) []map[string]string {
	if rows == nil {
		return []map[string]string{}
	}
	return rows
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}, "status": "failure"})
}
