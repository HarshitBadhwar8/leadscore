package hubspot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/logredact"
)

// client is one HubSpot API connection: the private-app token, the base URL
// and a single-shot call that maps HubSpot's answers to the engine's errors
// (contracts section 6, "Errors"). It never retries: a sink's 429 stops the
// sink for the run, and a lookup's failure makes its leads wait.
type client struct {
	base  string
	token string
	hc    *http.Client
	pace  *pacer // nil: no pacing (a fake behind base_url)
}

// apiError is a non-2xx answer. Error() carries the method, route, status and
// only the allowlisted machine ids of the body (logredact.VendorErrorDetail):
// HubSpot echoes property values, emails among them, in its messages. The
// raw body stays private, read only to find a 409's existing id or a 400's
// INVALID_EMAIL code.
type apiError struct {
	method, route string
	status        int
	body          []byte
	kind          error // api.ErrRateLimited, api.ErrTransient, api.ErrRefused or nil
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("hubspot: %s %s: status %d", e.method, e.route, e.status)
	if d := logredact.VendorErrorDetail(e.body); d != "" {
		s += " " + d
	}
	if e.status == http.StatusUnauthorized || e.status == http.StatusForbidden {
		s += " (HubSpot refused the token: check HUBSPOT_TOKEN and the private app's scopes; the sink stops for this run)"
	}
	return s
}

func (e *apiError) Unwrap() error { return e.kind }

// classify maps a status to the engine's error kinds: 429 is a rate limit,
// 5xx is transient, a 400 whose body names INVALID_EMAIL is a refusal that
// retrying cannot change (S0 confirms the code); anything else counts an
// attempt. A 401 or 403 (a revoked token, a missing scope) is reported as a
// rate limit: the sink stops for the run and no attempt is counted, since no
// lead is at fault; the `hubspot` check raises the problem.
func classify(status int, body []byte) error {
	switch {
	case status == http.StatusTooManyRequests, status == http.StatusUnauthorized, status == http.StatusForbidden:
		return api.ErrRateLimited
	case status >= 500:
		return api.ErrTransient
	case status == http.StatusBadRequest && bytes.Contains(body, []byte("INVALID_EMAIL")):
		return api.ErrRefused
	}
	return nil
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status
	}
	return 0
}

// existingIDPattern reads the contact id out of a 409's message, which reads
// "Contact already exists. Existing ID: 12345" (S0 confirms the wording; when
// it does not match, the sink searches by email instead).
var existingIDPattern = regexp.MustCompile(`Existing ID:\s*(\d+)`)

func existingID(err error) string {
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != http.StatusConflict {
		return ""
	}
	if m := existingIDPattern.FindSubmatch(ae.body); m != nil {
		return string(m[1])
	}
	return ""
}

// maxBody caps an answer read into memory; a larger one is treated as a
// failed read.
var maxBody int64 = 16 << 20

// call sends one request, bounded by callTimeout. A transport failure (a
// timeout, a refused connection) is ErrTransient, unless the caller's
// context ended, which is returned as is: the engine reads context.Canceled
// itself. Redirects are not followed (newClient), so the token never goes to
// another host.
func (c *client) call(parent context.Context, method, path string, in, out any) error {
	route, _, _ := strings.Cut(path, "?")
	if c.pace != nil && strings.HasSuffix(route, "/search") {
		if err := c.pace.wait(parent); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(parent, callTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("hubspot: encoding the %s request: %w", route, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("hubspot: building the %s request: %w", route, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return fmt.Errorf("hubspot: %s %s: %w", method, route, parent.Err())
		}
		// Only the route: a transport error quotes the URL, and the URL is
		// ours, but the error text from a proxy is not.
		return fmt.Errorf("hubspot: %s %s: the call did not complete: %w", method, route, api.ErrTransient)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return &apiError{method: method, route: route, status: resp.StatusCode, body: raw, kind: classify(resp.StatusCode, raw)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if parent.Err() != nil {
			return fmt.Errorf("hubspot: %s %s: %w", method, route, parent.Err())
		}
		return fmt.Errorf("hubspot: reading the %s answer: %w", route, api.ErrTransient)
	}
	if int64(len(raw)) > maxBody {
		return fmt.Errorf("hubspot: the %s answer is over %d bytes: %w", route, maxBody, api.ErrTransient)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hubspot: reading the %s answer: %w", route, api.ErrTransient)
	}
	return nil
}

// newClient builds the HTTP side: hc (a test client, or nil for a default
// one) copied with redirects turned off.
func newClient(base, token string, hc *http.Client) *client {
	h := http.Client{}
	if hc != nil {
		h = *hc
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{base: base, token: token, hc: &h}
}

// pacer spaces search calls: HubSpot limits its search endpoints to a few
// requests a second per account, well below the general limit (S0 confirms
// the number; 4 a second stays under the documented 5).
type pacer struct {
	mu    sync.Mutex
	next  time.Time
	every time.Duration
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(p.every)
	p.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// object is a CRM record as the v3 object, search and batch endpoints return
// it. A property HubSpot holds as null reads as absent.
type object struct {
	ID         string             `json:"id"`
	Properties map[string]*string `json:"properties"`
}

func (o object) prop(name string) string {
	if v := o.Properties[name]; v != nil {
		return *v
	}
	return ""
}

// filter is one search filter: EQ with Value, or IN with Values.
type filter struct {
	PropertyName string   `json:"propertyName"`
	Operator     string   `json:"operator"`
	Value        string   `json:"value,omitempty"`
	Values       []string `json:"values,omitempty"`
}

type filterGroup struct {
	Filters []filter `json:"filters"`
}

// Search limits (HubSpot's documented maximums): 200 results a page, 5
// filter groups, 100 values in an IN filter. maxSearchPages bounds one
// search; a search with more pages than that is reported as partial, never
// read as complete.
const (
	searchPageSize = 200
	maxGroups      = 5
	maxInValues    = 100
	maxSearchPages = 25
	batchSize      = 100 // inputs per batch read
	assocBatchSize = 1000
)

// errPartial is a read that could not see every result. A caller treats it
// like a failed read: nothing is concluded from it.
var errPartial = errors.New("hubspot: the search had more pages than one read follows")

// search reads every page of a search. The groups are ORed; filters within
// a group are ANDed.
func (c *client) search(ctx context.Context, objectType string, groups []filterGroup, props []string) ([]object, error) {
	var all []object
	after := ""
	for page := 0; page < maxSearchPages; page++ {
		in := map[string]any{"filterGroups": groups, "limit": searchPageSize, "properties": props}
		if after != "" {
			in["after"] = after
		}
		var out struct {
			Results []object `json:"results"`
			Paging  *struct {
				Next *struct {
					After string `json:"after"`
				} `json:"next"`
			} `json:"paging"`
		}
		if err := c.call(ctx, http.MethodPost, "/crm/v3/objects/"+objectType+"/search", in, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Results...)
		if out.Paging == nil || out.Paging.Next == nil || out.Paging.Next.After == "" {
			return all, nil
		}
		after = out.Paging.Next.After
	}
	return nil, errPartial
}

// batchError is one entry of a batch answer's errors.
type batchError struct {
	Category string `json:"category"`
	Context  struct {
		IDs []string `json:"ids"`
	} `json:"context"`
}

// notFound reports a batch error that only says some inputs do not exist:
// a deleted record, or an email no contact has.
func (e batchError) notFound() bool { return e.Category == "OBJECT_NOT_FOUND" }

// batchRead reads records by id (or by idProperty, such as email). Inputs
// that do not exist are left out of the answer; any other error fails the
// read (S0 confirms the 207 shape).
func (c *client) batchRead(ctx context.Context, objectType, idProperty string, ids, props []string) ([]object, error) {
	var all []object
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		inputs := make([]map[string]string, 0, end-start)
		for _, id := range ids[start:end] {
			inputs = append(inputs, map[string]string{"id": id})
		}
		in := map[string]any{"inputs": inputs, "properties": props}
		if idProperty != "" {
			in["idProperty"] = idProperty
		}
		var out struct {
			Results []object     `json:"results"`
			Errors  []batchError `json:"errors"`
		}
		if err := c.call(ctx, http.MethodPost, "/crm/v3/objects/"+objectType+"/batch/read", in, &out); err != nil {
			return nil, err
		}
		for _, e := range out.Errors {
			if !e.notFound() {
				return nil, fmt.Errorf("hubspot: reading %s: an input failed with %s: %w", objectType, clip(e.Category), api.ErrTransient)
			}
		}
		all = append(all, out.Results...)
	}
	return all, nil
}

// associations reads the ids of toType records associated with each
// fromType record (v4 batch read). A record with none is simply absent. An
// answer with more associations than one page is partial, and fails the
// read.
func (c *client) associations(ctx context.Context, fromType, toType string, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	for start := 0; start < len(ids); start += assocBatchSize {
		end := min(start+assocBatchSize, len(ids))
		inputs := make([]map[string]string, 0, end-start)
		for _, id := range ids[start:end] {
			inputs = append(inputs, map[string]string{"id": id})
		}
		var ans struct {
			Results []struct {
				From struct {
					ID string `json:"id"`
				} `json:"from"`
				To []struct {
					ToObjectID json.Number `json:"toObjectId"`
				} `json:"to"`
				Paging *struct {
					Next *struct {
						After string `json:"after"`
					} `json:"next"`
				} `json:"paging"`
			} `json:"results"`
			Errors []batchError `json:"errors"`
		}
		path := fmt.Sprintf("/crm/v4/associations/%s/%s/batch/read", fromType, toType)
		if err := c.call(ctx, http.MethodPost, path, map[string]any{"inputs": inputs}, &ans); err != nil {
			return nil, err
		}
		for _, e := range ans.Errors {
			// A record with no associations of the type is reported as an
			// error entry, not an empty result (S0 confirms the category).
			if !e.notFound() && e.Category != "NO_ASSOCIATIONS_FOUND" {
				return nil, fmt.Errorf("hubspot: reading %s-%s associations: an input failed with %s: %w", fromType, toType, clip(e.Category), api.ErrTransient)
			}
		}
		for _, r := range ans.Results {
			if r.Paging != nil && r.Paging.Next != nil && r.Paging.Next.After != "" {
				return nil, errPartial
			}
			for _, t := range r.To {
				out[r.From.ID] = append(out[r.From.ID], t.ToObjectID.String())
			}
		}
	}
	return out, nil
}

// create makes one record with its properties and, optionally, its
// associations in the same call, and returns its id.
func (c *client) create(ctx context.Context, objectType string, props map[string]string, assoc []any) (string, error) {
	in := map[string]any{"properties": props}
	if len(assoc) > 0 {
		in["associations"] = assoc
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, "/crm/v3/objects/"+objectType, in, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("hubspot: creating a %s returned no id: %w", strings.TrimSuffix(objectType, "s"), api.ErrTransient)
	}
	return out.ID, nil
}

// clip keeps a vendor-supplied token short and printable for an error.
func clip(s string) string {
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
