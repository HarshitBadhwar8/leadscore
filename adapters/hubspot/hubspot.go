// Package hubspot is the HubSpot adapter (RFC 6.11, 6.12; contracts section
// 6): the `hubspot` sink, which finds or creates contacts and one deal per
// company; the HubSpot Lookup, which reads opt-outs and each company's deals
// before pushing; `leadscore setup hubspot`; and the `hubspot` check.
//
// Configured under sinks.hubspot in leadscore.yml:
//
//	sinks:
//	  hubspot: { pipeline: Sales Pipeline, stage: Appointment scheduled }
//
// The private-app token is read from HUBSPOT_TOKEN. `property_prefix`
// (default leadscore_) names the custom properties setup creates.
//
// Request and answer shapes are provisional: they come from HubSpot's public
// API documentation and earlier working code, not from recorded calls. Every
// behaviour that rests on an answer still to be checked is marked
// "S0 confirms" (RFC 10).
package hubspot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
	"github.com/HarshitBadhwar8/leadscore/internal/check"
)

func init() {
	api.RegisterSink("hubspot", func(cfg api.Config) (api.Sink, error) { return NewSink(cfg) })
	api.RegisterLookup("hubspot", func(cfg api.Config) (api.Lookup, error) { return NewLookup(cfg) })
	check.Register(hubspotCheck{})
}

// callTimeout bounds every call (contracts section 11); a variable so a test
// can shorten it.
var callTimeout = 30 * time.Second

// TokenVariable holds the private-app token.
const TokenVariable = "HUBSPOT_TOKEN"

const (
	defaultBaseURL = "https://api.hubapi.com"
	defaultPrefix  = "leadscore_"
	// searchEvery spaces search calls against the real API.
	searchEvery = 250 * time.Millisecond
	// groupName is the property group setup creates on contacts and deals.
	groupName = "leadscore"
)

// Destinations and their steps (contracts section 6).
const (
	destContacts = "contacts"
	destDeals    = "deals"
	stepContact  = "contact"
	stepDeal     = "deal"
)

var prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// settings is one sinks.hubspot block, read once.
type settings struct {
	pipeline, stage string // names, resolved to ids per run
	prefix          string
	c               *client
}

// parse reads a sinks.hubspot block. `base_url` and `_http_client` are for
// tests only: a `base_url` without a test client is refused, so a file can
// never send the token to another host.
func parse(cfg api.Config) (settings, error) {
	var s settings
	str := func(key string) (string, error) {
		v, has := cfg[key]
		if !has || v == nil {
			return "", nil
		}
		t, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("sinks.hubspot.%s must be text (quote it)", key)
		}
		return strings.TrimSpace(t), nil
	}
	var err error
	if s.pipeline, err = str("pipeline"); err != nil {
		return s, err
	}
	if s.stage, err = str("stage"); err != nil {
		return s, err
	}
	if s.prefix, err = str("property_prefix"); err != nil {
		return s, err
	}
	if s.prefix == "" {
		s.prefix = defaultPrefix
	}
	if !prefixPattern.MatchString(s.prefix) {
		return s, fmt.Errorf("sinks.hubspot.property_prefix %q must be lowercase letters, digits and _, starting with a letter", s.prefix)
	}
	if (s.pipeline == "") != (s.stage == "") {
		return s, errors.New("sinks.hubspot.pipeline and sinks.hubspot.stage are set together")
	}
	token := strings.TrimSpace(os.Getenv(TokenVariable))
	if token == "" {
		return s, fmt.Errorf("%s is not set", TokenVariable)
	}
	base, err := str("base_url")
	if err != nil {
		return s, err
	}
	hc, _ := cfg["_http_client"].(*http.Client)
	if base != "" {
		if hc == nil {
			return s, errors.New("sinks.hubspot.base_url is for tests only and needs a test HTTP client; remove it from leadscore.yml")
		}
		s.c = newClient(strings.TrimRight(base, "/"), token, hc)
	} else {
		s.c = newClient(defaultBaseURL, token, hc)
		s.c.pace = &pacer{every: searchEvery}
	}
	return s, nil
}

func (s settings) prop(name string) string { return s.prefix + name }

// stageClass is a deal stage's class: open, won or lost (contracts section
// 6, "Deal stages").
type stageClass string

const (
	classOpen stageClass = "open"
	classWon  stageClass = "won"
	classLost stageClass = "lost"
)

// pipelines is the portal's deal pipelines: each stage's class by pipeline
// and stage id, and the ids the configured names resolve to.
type pipelines struct {
	class               map[string]map[string]stageClass // pipeline id -> stage id -> class
	pipelineID, stageID string                           // the configured pipeline and stage; empty when unset
}

type pipelineList struct {
	Results []struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Stages []struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			Metadata struct {
				IsClosed    string `json:"isClosed"`
				Probability string `json:"probability"`
			} `json:"metadata"`
		} `json:"stages"`
	} `json:"results"`
}

// errConfig marks a pipeline or stage name that does not resolve: a deal
// step waits (ErrTransient) until the name is fixed, and the `hubspot` check
// names it.
var errConfig = errors.New("sinks.hubspot names a pipeline or stage the portal does not have")

// readPipelines reads every deal pipeline and resolves the configured names
// (case-insensitive, trimmed; an ambiguous name is refused, as is a missing
// one).
func readPipelines(ctx context.Context, s settings) (*pipelines, error) {
	var out pipelineList
	if err := s.c.call(ctx, http.MethodGet, "/crm/v3/pipelines/deals", nil, &out); err != nil {
		return nil, fmt.Errorf("reading the deal pipelines: %w", err)
	}
	p := &pipelines{class: map[string]map[string]stageClass{}}
	matches := 0
	for _, pl := range out.Results {
		stages := map[string]stageClass{}
		for _, st := range pl.Stages {
			stages[st.ID] = classify3(st.Metadata.IsClosed, st.Metadata.Probability)
		}
		p.class[pl.ID] = stages
		if s.pipeline == "" || !strings.EqualFold(strings.TrimSpace(pl.Label), s.pipeline) {
			continue
		}
		matches++
		p.pipelineID, p.stageID = pl.ID, ""
		n := 0
		for _, st := range pl.Stages {
			if strings.EqualFold(strings.TrimSpace(st.Label), s.stage) {
				n++
				p.stageID = st.ID
			}
		}
		if n > 1 {
			return p, fmt.Errorf("%w: pipeline %q has %d stages named %q", errConfig, s.pipeline, n, s.stage)
		}
	}
	switch {
	case s.pipeline == "":
	case matches == 0:
		return p, fmt.Errorf("%w: no deal pipeline is named %q", errConfig, s.pipeline)
	case matches > 1:
		return p, fmt.Errorf("%w: %d deal pipelines are named %q", errConfig, matches, s.pipeline)
	case p.stageID == "":
		return p, fmt.Errorf("%w: pipeline %q has no stage named %q", errConfig, s.pipeline, s.stage)
	}
	if s.pipeline != "" && p.class[p.pipelineID][p.stageID] != classOpen {
		return p, fmt.Errorf("%w: stage %q of pipeline %q is a closed stage; new deals need an open one", errConfig, s.stage, s.pipeline)
	}
	return p, nil
}

// classify3 reads a stage's metadata: not closed is open; closed with
// probability 1 is won; any other closed stage is lost. Metadata it cannot
// read counts as open, which holds the company: the safe direction. S0
// confirms the metadata keys (isClosed, probability) and their text values.
func classify3(isClosed, probability string) stageClass {
	if !strings.EqualFold(strings.TrimSpace(isClosed), "true") {
		return classOpen
	}
	p, err := strconv.ParseFloat(strings.TrimSpace(probability), 64)
	if err != nil {
		return classOpen
	}
	if p >= 1 {
		return classWon
	}
	return classLost
}

// of returns a deal's stage class. A stage no pipeline lists counts as open
// (known reports false), so the lookup holds the company; the sink never
// reuses such a deal.
func (p *pipelines) of(pipelineID, stageID string) (c stageClass, known bool) {
	if c, ok := p.class[pipelineID][stageID]; ok {
		return c, true
	}
	return classOpen, false
}
