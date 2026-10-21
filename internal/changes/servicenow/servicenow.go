// Package servicenow is the ServiceNow provider for the change-management
// bridge. It opens change requests with the Change Management API and reads
// and closes them with the Table API.
package servicenow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeamber/kubeamber/internal/changerequest"
	"github.com/kubeamber/kubeamber/internal/changes"
	"github.com/kubeamber/kubeamber/pkg/policy"
)

// Name is the system name recorded on change requests and approvals.
const Name = "servicenow"

// Config configures the ServiceNow provider.
type Config struct {
	// InstanceURL is the instance root, e.g. https://example.service-now.com.
	InstanceURL string `json:"instanceURL"`
	Auth        Auth   `json:"auth"`
	// ChangeType is normal (default) or emergency.
	ChangeType string `json:"changeType,omitempty"`
	// Window is the planned window opened from the moment the ticket is
	// created. The change can be applied until it ends. Default 24h.
	Window metav1.Duration `json:"window,omitempty"`
	// Fields are set on every ticket, e.g. assignment_group or category.
	Fields map[string]string `json:"fields,omitempty"`
	// ConfigurationItems maps namespace globs to cmdb_ci values. The most
	// specific (longest) matching pattern wins, so "*" can be a default.
	ConfigurationItems map[string]string `json:"configurationItems,omitempty"`
	// CloseState is review (default) or closed.
	CloseState string          `json:"closeState,omitempty"`
	Timeout    metav1.Duration `json:"timeout,omitempty"`
}

// Auth selects how the bridge authenticates. Credentials are read from files
// (mounted from a Kubernetes Secret) on every use, so they can be rotated
// without a restart and never appear in configuration.
type Auth struct {
	// Type is oauth (client credentials, preferred) or basic.
	Type             string `json:"type"`
	ClientIDFile     string `json:"clientIDFile,omitempty"`
	ClientSecretFile string `json:"clientSecretFile,omitempty"`
	UsernameFile     string `json:"usernameFile,omitempty"`
	PasswordFile     string `json:"passwordFile,omitempty"`
}

// Default change model states.
const (
	stateNew       = "-5"
	stateAssess    = "-4"
	stateAuthorize = "-3"
	stateScheduled = "-2"
	stateImplement = "-1"
	stateReview    = "0"
	stateClosed    = "3"
	stateCanceled  = "4"
)

var stateNames = map[string]string{
	stateNew: "New", stateAssess: "Assess", stateAuthorize: "Authorize", stateScheduled: "Scheduled",
	stateImplement: "Implement", stateReview: "Review", stateClosed: "Closed", stateCanceled: "Canceled",
}

// glideTime is ServiceNow's date-time format; values are UTC.
const glideTime = "2006-01-02 15:04:05"

// Client implements changes.Provider for ServiceNow.
type Client struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

var _ changes.Provider = (*Client)(nil)

// New validates the configuration and returns a client.
func New(cfg Config) (*Client, error) {
	cfg.InstanceURL = strings.TrimRight(cfg.InstanceURL, "/")
	u, err := url.Parse(cfg.InstanceURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("servicenow.instanceURL %q is not an http(s) URL", cfg.InstanceURL)
	}
	if cfg.ChangeType == "" {
		cfg.ChangeType = "normal"
	}
	if cfg.ChangeType != "normal" && cfg.ChangeType != "emergency" {
		return nil, fmt.Errorf("servicenow.changeType must be normal or emergency")
	}
	if cfg.Window.Duration == 0 {
		cfg.Window.Duration = 24 * time.Hour
	}
	if cfg.CloseState == "" {
		cfg.CloseState = "review"
	}
	if cfg.CloseState != "review" && cfg.CloseState != "closed" {
		return nil, fmt.Errorf("servicenow.closeState must be review or closed")
	}
	if cfg.Timeout.Duration == 0 {
		cfg.Timeout.Duration = 15 * time.Second
	}
	switch cfg.Auth.Type {
	case "oauth":
		if cfg.Auth.ClientIDFile == "" || cfg.Auth.ClientSecretFile == "" {
			return nil, fmt.Errorf("servicenow.auth: oauth needs clientIDFile and clientSecretFile")
		}
	case "basic":
		if cfg.Auth.UsernameFile == "" || cfg.Auth.PasswordFile == "" {
			return nil, fmt.Errorf("servicenow.auth: basic needs usernameFile and passwordFile")
		}
	default:
		return nil, fmt.Errorf("servicenow.auth.type must be oauth or basic")
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout.Duration}, now: time.Now}, nil
}

func (c *Client) Name() string { return Name }

// Open finds the ticket already opened for this change request (by
// correlation_id) or creates one.
func (c *Client) Open(ctx context.Context, cr *changerequest.ChangeRequest) (*changerequest.External, error) {
	q := url.Values{
		"sysparm_query":  {"correlation_id=" + cr.Name},
		"sysparm_fields": {"sys_id,number,state,start_date,end_date"},
		"sysparm_limit":  {"1"},
	}
	var found struct {
		Result []map[string]any `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/now/table/change_request?"+q.Encode(), nil, &found); err != nil {
		return nil, err
	}
	if len(found.Result) > 0 {
		return c.external(found.Result[0])
	}

	start := c.now().UTC().Truncate(time.Second)
	body := map[string]string{
		"short_description":   shortDescription(cr),
		"description":         description(cr),
		"justification":       justification(cr),
		"correlation_id":      cr.Name,
		"correlation_display": "KubeAmber",
		"start_date":          start.Format(glideTime),
		"end_date":            start.Add(c.cfg.Window.Duration).Format(glideTime),
	}
	if ci := c.configurationItem(cr.Spec.Target.Namespace); ci != "" {
		body["cmdb_ci"] = ci
	}
	for k, v := range c.cfg.Fields {
		body[k] = v
	}
	var created struct {
		Result map[string]any `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/sn_chg_rest/change/"+c.cfg.ChangeType, body, &created); err != nil {
		return nil, err
	}
	return c.external(created.Result)
}

// Sync reads the ticket's state, window and decision.
func (c *Client) Sync(ctx context.Context, ext changerequest.External) (changes.Ticket, error) {
	q := url.Values{"sysparm_fields": {"number,state,approval,start_date,end_date"}}
	var got struct {
		Result map[string]any `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/now/table/change_request/"+url.PathEscape(ext.ID)+"?"+q.Encode(), nil, &got); err != nil {
		return changes.Ticket{}, err
	}
	r := got.Result
	state := field(r, "state")
	t := changes.Ticket{State: stateName(state), PlannedStart: glide(field(r, "start_date")), PlannedEnd: glide(field(r, "end_date"))}

	switch approval := field(r, "approval"); {
	case approval == "rejected" || state == stateCanceled:
		t.Decision = changerequest.DecisionRejected
		users, err := c.approvers(ctx, ext.ID, "rejected")
		if err != nil {
			return t, err
		}
		t.DecidedBy = users
		verb := "rejected"
		if state == stateCanceled {
			verb = "cancelled"
		}
		t.Reason = fmt.Sprintf("%s %s in ServiceNow", ext.Number, verb)
	case approval == "approved" && approvedState(state):
		t.Decision = changerequest.DecisionApproved
		users, err := c.approvers(ctx, ext.ID, "approved")
		if err != nil {
			return t, err
		}
		t.DecidedBy = users
		t.Reason = fmt.Sprintf("%s approved in ServiceNow", ext.Number)
	}
	return t, nil
}

func approvedState(s string) bool {
	switch s {
	case stateScheduled, stateImplement, stateReview, stateClosed:
		return true
	}
	return false
}

// approvers lists the user names on the ticket's approval records in state.
func (c *Client) approvers(ctx context.Context, sysID, state string) ([]string, error) {
	q := url.Values{
		"sysparm_query":  {"sysapproval=" + sysID + "^state=" + state},
		"sysparm_fields": {"approver.user_name"},
	}
	var got struct {
		Result []map[string]any `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/now/table/sysapproval_approver?"+q.Encode(), nil, &got); err != nil {
		return nil, err
	}
	var users []string
	for _, r := range got.Result {
		if u := field(r, "approver.user_name"); u != "" {
			users = append(users, u)
		}
	}
	sort.Strings(users)
	return users, nil
}

// Close records the outcome. An implemented change moves through Implement
// to Review (or Closed) with close code successful; an abandoned one is
// cancelled. Both get a work note.
func (c *Client) Close(ctx context.Context, ext changerequest.External, outcome changes.Outcome, notes string) error {
	path := "/api/now/table/change_request/" + url.PathEscape(ext.ID)
	if outcome == changes.Abandoned {
		return c.do(ctx, http.MethodPatch, path, map[string]string{"state": stateCanceled, "work_notes": notes}, nil)
	}
	// The default change model does not allow skipping Implement.
	if err := c.do(ctx, http.MethodPatch, path, map[string]string{"state": stateImplement, "work_notes": notes}, nil); err != nil {
		return err
	}
	final := stateReview
	if c.cfg.CloseState == "closed" {
		final = stateClosed
	}
	return c.do(ctx, http.MethodPatch, path, map[string]string{
		"state": final, "close_code": "successful", "close_notes": notes,
	}, nil)
}

func (c *Client) external(r map[string]any) (*changerequest.External, error) {
	id, number := field(r, "sys_id"), field(r, "number")
	if id == "" || number == "" {
		return nil, fmt.Errorf("servicenow returned a change request without sys_id or number")
	}
	return &changerequest.External{
		System:       Name,
		ID:           id,
		Number:       number,
		URL:          c.cfg.InstanceURL + "/change_request.do?sys_id=" + url.QueryEscape(id),
		State:        stateName(field(r, "state")),
		PlannedStart: metaTime(glide(field(r, "start_date"))),
		PlannedEnd:   metaTime(glide(field(r, "end_date"))),
	}, nil
}

func (c *Client) configurationItem(ns string) string {
	patterns := make([]string, 0, len(c.cfg.ConfigurationItems))
	for p := range c.cfg.ConfigurationItems {
		patterns = append(patterns, p)
	}
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i]) != len(patterns[j]) {
			return len(patterns[i]) > len(patterns[j])
		}
		return patterns[i] < patterns[j]
	})
	for _, p := range patterns {
		if policy.Glob(p, ns) {
			return c.cfg.ConfigurationItems[p]
		}
	}
	return ""
}

// do sends a request and decodes a JSON response into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.InstanceURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("servicenow %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = "" // force a new OAuth token next time
		c.mu.Unlock()
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("servicenow %s %s: %s: %s", method, strings.SplitN(path, "?", 2)[0], resp.Status, errorMessage(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("servicenow %s %s: decoding response: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

func (c *Client) authorize(ctx context.Context, req *http.Request) error {
	a := c.cfg.Auth
	if a.Type == "basic" {
		user, err := readSecret(a.UsernameFile)
		if err != nil {
			return err
		}
		pass, err := readSecret(a.PasswordFile)
		if err != nil {
			return err
		}
		req.SetBasicAuth(user, pass)
		return nil
	}
	token, err := c.oauthToken(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// oauthToken returns a cached client-credentials token, fetching a new one a
// minute before the old one expires.
func (c *Client) oauthToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.tokenExpiry) {
		return c.token, nil
	}
	id, err := readSecret(c.cfg.Auth.ClientIDFile)
	if err != nil {
		return "", err
	}
	secret, err := readSecret(c.cfg.Auth.ClientSecretFile)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.InstanceURL+"/oauth_token.do", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("servicenow oauth: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("servicenow oauth: %s: %s", resp.Status, errorMessage(data))
	}
	var tok struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("servicenow oauth: no access_token in response")
	}
	ttl, _ := tok.ExpiresIn.Int64()
	if ttl <= 0 {
		ttl = 1800
	}
	c.token = tok.AccessToken
	c.tokenExpiry = c.now().Add(time.Duration(ttl)*time.Second - time.Minute)
	return c.token, nil
}

func readSecret(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading credential: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// field reads a value that ServiceNow returns either as a string or, from
// the Change Management API and for reference fields, as {"value": ...}.
func field(r map[string]any, key string) string {
	switch v := r[key].(type) {
	case string:
		return v
	case map[string]any:
		s, _ := v["value"].(string)
		return s
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func errorMessage(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
		if e.Error.Detail != "" {
			return e.Error.Message + ": " + e.Error.Detail
		}
		return e.Error.Message
	}
	s := strings.TrimSpace(string(data))
	return s[:min(200, len(s))]
}

func stateName(s string) string {
	if n, ok := stateNames[s]; ok {
		return n
	}
	return s
}

func glide(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.ParseInLocation(glideTime, s, time.UTC)
	if err != nil {
		return nil
	}
	return &t
}

func metaTime(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	m := metav1.NewTime(*t)
	return &m
}

func target(cr *changerequest.ChangeRequest) string {
	t := cr.Spec.Target
	if t.Namespace != "" {
		return fmt.Sprintf("%s %s/%s", t.Kind, t.Namespace, t.Name)
	}
	return fmt.Sprintf("%s %s", t.Kind, t.Name)
}

func shortDescription(cr *changerequest.ChangeRequest) string {
	s := fmt.Sprintf("AI agent %s: %s %s", cr.Spec.Agent, cr.Spec.Operation, target(cr))
	if cr.Spec.Summary != "" {
		s += " (" + cr.Spec.Summary + ")"
	}
	return s[:min(160, len(s))]
}

func justification(cr *changerequest.ChangeRequest) string {
	if cr.Spec.Intent != "" {
		return "Agent's stated intent: " + cr.Spec.Intent
	}
	return "The agent gave no intent. Ask its owner before approving."
}

// description carries everything an approver needs, and nothing from the
// object itself: no Secret data or environment values leave the cluster.
func description(cr *changerequest.ChangeRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "An AI agent's change to Kubernetes was held by KubeAmber for approval.\n\n")
	fmt.Fprintf(&b, "Agent: %s (identity %s)\n", cr.Spec.Agent, cr.Spec.User)
	fmt.Fprintf(&b, "Change: %s %s", cr.Spec.Operation, target(cr))
	if cr.Spec.Target.SubResource != "" {
		fmt.Fprintf(&b, " [%s]", cr.Spec.Target.SubResource)
	}
	b.WriteString("\n")
	if cr.Spec.Summary != "" {
		fmt.Fprintf(&b, "What changes: %s\n", cr.Spec.Summary)
	}
	if cr.Spec.Intent != "" {
		fmt.Fprintf(&b, "Agent's stated intent: %s\n", cr.Spec.Intent)
	}
	fmt.Fprintf(&b, "Risk score: %d/100\n", cr.Spec.Score)
	for _, f := range cr.Spec.Factors {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	for _, r := range cr.Spec.Reasons {
		fmt.Fprintf(&b, "Decision: %s\n", r)
	}
	fmt.Fprintf(&b, "\nKubeAmber change request: %s\nFingerprint: %s\n", cr.Name, cr.Spec.Fingerprint)
	if cr.Spec.TraceParent != "" {
		fmt.Fprintf(&b, "Agent trace (W3C traceparent): %s\n", cr.Spec.TraceParent)
	}
	b.WriteString("\nApproving lets the agent apply exactly this change, once, within the planned window.")
	return b.String()
}
