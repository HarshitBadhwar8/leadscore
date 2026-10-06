// Package config loads leadscore.yml (contracts section 3): every engine key
// with its default, the adapter blocks passed through as api.Config, the default
// file locations, the hosted bundle, and the rubric path. It also backs the
// `config get` and `config set-hosting` commands.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Default locations (section 3). Variables so tests can point them elsewhere.
var (
	defaultBundlePath = "/config/bundle.yaml"
	defaultConfigPath = "/config/leadscore.yml"
	localConfigPath   = "leadscore.yml"
)

// Defaults, as section 3 and section 11 write them. Durations stay in their
// written form so `config get` prints what a person would type.
const (
	DefaultRubricFile      = "rubric.yml"
	DefaultSQLitePath      = "/data/leadscore.db"
	DefaultEnrichMaxAge    = "30d"
	DefaultLookupsPerRun   = 100
	DefaultLookupsPerDay   = 400
	DefaultReplies         = "receiver"
	DefaultSequenceLength  = "30d"
	DefaultWindowMargin    = "7d"
	DefaultReceiverPort    = 8080
	DefaultPropertyPrefix  = "leadscore_"
	DefaultExportDir       = "/out"
	DefaultSchedule        = "15m"
	DefaultDeadline        = "12m"
	DefaultIngestChunkRows = 2000
	DefaultSilence         = "3d"
	DefaultLogRetention    = "90d"
)

// Engine keys. Anything else at these levels fails loading, naming the key.
// Adapter blocks (store, sources[], enrich, sinks.<type>) are passed through, so
// a plug-in may read keys the engine does not know.
var (
	topLevelKeys = set("version", "rubric", "store", "sources", "enrich", "replies", "polling",
		"receiver", "sinks", "export", "reply_labels", "pushes_enabled", "schedule", "deadline",
		"ingest_chunk_rows", "silence_threshold", "log_retention", "hosting")
	pollingKeys  = set("sequence_length", "window_margin")
	receiverKeys = set("public_url", "visit_events", "port")
	exportKeys   = set("dir")
	// HostingKeys are the keys `config set-hosting` may write.
	HostingKeys = []string{"project", "region", "run_account", "receiver_account", "image"}
	hostingKeys = set(HostingKeys...)
	bundleKeys  = set("config", "rubric")
	// Section 5.5: teams may map a label to a replied_* status or to none.
	replyLabelValues = set("replied_positive", "replied_negative", "replied_neutral", "replied_unlabelled", "none")
)

// Config is a loaded leadscore.yml with every default applied and every
// relative path resolved against the folder holding the file.
type Config struct {
	Path   string // the file read: leadscore.yml, or the hosted bundle
	Dir    string // the folder relative paths resolve against
	Bundle bool   // loaded from a hosted bundle (section 3)

	Version    int
	RubricPath string // resolved rubric file; empty for a bundle, which carries the text
	RubricText []byte // the bundle's rubric; nil when RubricPath is used

	Store            Store
	Sources          []Source
	Enrich           *Enrich // nil when no enrich block
	Replies          string  // "receiver" or "polling"
	Polling          Polling
	Receiver         Receiver
	Sinks            map[string]api.Config // sinks.<type>, defaults applied; never nil
	Export           Export
	ReplyLabels      map[string]string // the team's per-label overrides only; the base map is section 5.5
	PushesEnabled    bool
	Schedule         time.Duration
	Deadline         time.Duration
	IngestChunkRows  int
	SilenceThreshold time.Duration
	LogRetention     time.Duration
	Hosting          *Hosting // nil when no hosting block

	eff map[string]any // the effective document, for Get
}

type Store struct {
	Type            string
	Path            string // SQLite file; DefaultSQLitePath for sqlite when unset
	Spreadsheet     string
	LeaseBucket     string
	ViewSpreadsheet string
	Credentials     string     // empty means Google's standard credential loading
	Block           api.Config // the whole store block, for the backend factory
}

type Source struct {
	ID              string
	Type            string
	Channel         string // defaults to ID; what sources_seen counts
	Events          bool
	ApolloHeld      bool
	MatchDomainName bool
	Block           api.Config // the whole entry, for the source factory
}

type Enrich struct {
	Type             string
	MaxAge           time.Duration
	MaxLookupsPerRun int
	MaxLookupsPerDay int
	Block            api.Config
}

type Polling struct {
	SequenceLength time.Duration
	WindowMargin   time.Duration
}

type Receiver struct {
	PublicURL   string
	VisitEvents []string
	Port        int
}

type Export struct {
	Dir string
}

type Hosting struct {
	Project, Region, RunAccount, ReceiverAccount, Image string
}

// Options says where to load from. Empty paths mean the section 3 defaults.
type Options struct {
	ConfigPath string // --config
	RubricPath string // --rubric; ignored for a bundle
	// Getenv reads $PORT; nil means os.Getenv.
	Getenv func(string) string
}

// Locate returns the file to load: the flag when given; else /config/bundle.yaml
// when it exists; else /config/leadscore.yml when it exists; else ./leadscore.yml.
func Locate(configFlag string) (path string, err error) {
	if configFlag != "" {
		return filepath.Abs(configFlag)
	}
	for _, p := range []string{defaultBundlePath, defaultConfigPath} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return filepath.Abs(localConfigPath)
}

// Load locates, reads and parses the configuration.
func Load(opts Options) (*Config, error) {
	path, err := Locate(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("locating leadscore.yml: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	dir := filepath.Dir(path)

	text, rubric, isBundle, err := splitBundle(data, path)
	if err != nil {
		return nil, err
	}
	c, err := parse(text, dir, opts.Getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	c.Bundle = isBundle
	switch {
	case isBundle:
		c.RubricPath = ""
		c.RubricText = rubric
		delete(c.eff, "rubric")
	case opts.RubricPath != "":
		abs, err := filepath.Abs(opts.RubricPath)
		if err != nil {
			return nil, fmt.Errorf("resolving --rubric: %w", err)
		}
		c.RubricPath = abs
		c.eff["rubric"] = abs
	}
	return c, nil
}

// Parse reads leadscore.yml text, resolving relative paths against dir.
func Parse(data []byte, dir string, getenv func(string) string) (*Config, error) {
	return parse(data, dir, getenv)
}

// Rubric returns the rubric text: the bundle's, or the file at RubricPath.
func (c *Config) Rubric() ([]byte, error) {
	if c.Bundle {
		return c.RubricText, nil
	}
	b, err := os.ReadFile(c.RubricPath)
	if err != nil {
		return nil, fmt.Errorf("reading rubric: %w", err)
	}
	return b, nil
}

// splitBundle tells a hosted bundle from a plain leadscore.yml. A bundle is a
// mapping with exactly the keys `config` and `rubric`, each holding a file's
// text; a leadscore.yml always has `version`, so the two cannot be confused.
func splitBundle(data []byte, path string) (cfg, rubric []byte, isBundle bool, err error) {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, nil, false, fmt.Errorf("%s: %w", path, err)
	}
	if _, hasConfig := top["config"]; !hasConfig {
		return data, nil, false, nil
	}
	if _, hasVersion := top["version"]; hasVersion {
		return data, nil, false, nil // a leadscore.yml with a stray `config` key; parse names it
	}
	for k := range top {
		if !bundleKeys[k] {
			return nil, nil, false, fmt.Errorf("%s: hosted bundle has unknown key %q", path, k)
		}
	}
	c, ok1 := top["config"].(string)
	r, ok2 := top["rubric"].(string)
	if !ok1 || !ok2 {
		return nil, nil, false, fmt.Errorf("%s: hosted bundle needs `config` and `rubric`, each holding a file's text", path)
	}
	return []byte(c), []byte(r), true, nil
}

func parse(data []byte, dir string, getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("empty file: `version` and `store` are required")
	}
	if err := unknownKeys(raw, topLevelKeys, ""); err != nil {
		return nil, err
	}
	p := &parser{dir: dir}
	c := &Config{Dir: dir, eff: raw}

	// version: required, 1.
	v, ok := raw["version"]
	if !ok {
		return nil, errors.New("`version` is required (use 1)")
	}
	if n, isInt := v.(int); !isInt || n != 1 {
		return nil, fmt.Errorf("`version` must be 1, got %v", v)
	}
	c.Version = 1

	// rubric: rubric.yml beside leadscore.yml.
	rubric := p.str(raw, "rubric", "rubric", DefaultRubricFile)
	c.RubricPath = p.path(rubric)
	raw["rubric"] = c.RubricPath

	if err := p.store(c, raw); err != nil {
		return nil, err
	}
	if err := p.sources(c, raw); err != nil {
		return nil, err
	}
	if err := p.enrich(c, raw); err != nil {
		return nil, err
	}

	c.Replies = p.str(raw, "replies", "replies", DefaultReplies)
	if c.Replies != "receiver" && c.Replies != "polling" {
		p.fail("`replies` must be receiver or polling, got %q", c.Replies)
	}

	polling := p.block(raw, "polling", pollingKeys)
	c.Polling.SequenceLength = p.duration(polling, "sequence_length", "polling.sequence_length", DefaultSequenceLength)
	c.Polling.WindowMargin = p.duration(polling, "window_margin", "polling.window_margin", DefaultWindowMargin)

	if err := p.receiver(c, raw, getenv); err != nil {
		return nil, err
	}
	if err := p.sinks(c, raw); err != nil {
		return nil, err
	}

	export := p.block(raw, "export", exportKeys)
	c.Export.Dir = p.path(p.str(export, "dir", "export.dir", DefaultExportDir))
	export["dir"] = c.Export.Dir

	p.replyLabels(c, raw)

	c.PushesEnabled = p.boolean(raw, "pushes_enabled", "pushes_enabled")
	c.Schedule = p.duration(raw, "schedule", "schedule", DefaultSchedule)
	c.Deadline = p.duration(raw, "deadline", "deadline", DefaultDeadline)
	c.IngestChunkRows = p.positiveInt(raw, "ingest_chunk_rows", "ingest_chunk_rows", DefaultIngestChunkRows)
	c.SilenceThreshold = p.duration(raw, "silence_threshold", "silence_threshold", DefaultSilence)
	c.LogRetention = p.duration(raw, "log_retention", "log_retention", DefaultLogRetention)
	// A zero here would mean a run with no time, a timer that spins, or a log
	// trimmed as soon as it is written.
	for _, d := range []struct {
		name string
		v    time.Duration
	}{
		{"schedule", c.Schedule}, {"deadline", c.Deadline},
		{"silence_threshold", c.SilenceThreshold}, {"log_retention", c.LogRetention},
	} {
		if d.v <= 0 {
			p.fail("`%s` must be longer than zero", d.name)
		}
	}

	if _, has := raw["hosting"]; has {
		h := p.block(raw, "hosting", hostingKeys)
		c.Hosting = &Hosting{
			Project:         p.str(h, "project", "hosting.project", ""),
			Region:          p.str(h, "region", "hosting.region", ""),
			RunAccount:      p.str(h, "run_account", "hosting.run_account", ""),
			ReceiverAccount: p.str(h, "receiver_account", "hosting.receiver_account", ""),
			Image:           p.str(h, "image", "hosting.image", ""),
		}
	}

	if p.err != nil {
		return nil, p.err
	}
	return c, nil
}

func (p *parser) store(c *Config, raw map[string]any) error {
	s, ok := raw["store"].(map[string]any)
	if !ok {
		return errors.New("`store` is required, with a `type` (sqlite, sheets, or a plug-in)")
	}
	c.Store.Type = p.str(s, "type", "store.type", "")
	if c.Store.Type == "" && p.err == nil {
		return errors.New("`store.type` is required (sqlite, sheets, or a plug-in)")
	}
	if c.Store.Type == "sqlite" {
		c.Store.Path = p.path(p.str(s, "path", "store.path", DefaultSQLitePath))
		s["path"] = c.Store.Path
	} else if _, has := s["path"]; has {
		c.Store.Path = p.path(p.str(s, "path", "store.path", ""))
		s["path"] = c.Store.Path
	}
	c.Store.Spreadsheet = p.str(s, "spreadsheet", "store.spreadsheet", "")
	c.Store.LeaseBucket = p.str(s, "lease_bucket", "store.lease_bucket", "")
	c.Store.ViewSpreadsheet = p.str(s, "view_spreadsheet", "store.view_spreadsheet", "")
	if cred := p.str(s, "credentials", "store.credentials", ""); cred != "" {
		c.Store.Credentials = p.path(cred)
		s["credentials"] = c.Store.Credentials
	}
	c.Store.Block = deepCopy(s).(map[string]any)
	return p.err
}

func (p *parser) sources(c *Config, raw map[string]any) error {
	v, has := raw["sources"]
	if !has || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return errors.New("`sources` must be a list")
	}
	seen := map[string]bool{}
	for i, item := range list {
		e, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("`sources[%d]` must be a mapping", i)
		}
		name := fmt.Sprintf("sources[%d]", i)
		s := Source{
			ID:   p.str(e, "id", name+".id", ""),
			Type: p.str(e, "type", name+".type", ""),
		}
		if s.ID == "" && p.err == nil {
			return fmt.Errorf("`%s.id` is required", name)
		}
		if seen[s.ID] {
			return fmt.Errorf("`%s.id` %q is used by another source", name, s.ID)
		}
		seen[s.ID] = true
		s.Channel = p.str(e, "channel", name+".channel", s.ID)
		e["channel"] = s.Channel
		s.Events = p.boolean(e, "events", name+".events")
		s.ApolloHeld = p.boolean(e, "apollo_held", name+".apollo_held")
		s.MatchDomainName = p.boolean(e, "match_domain_name", name+".match_domain_name")
		if path, isStr := e["path"].(string); isStr && path != "" {
			e["path"] = p.path(path)
		}
		// The engine hands a Sheet-tab source the store's spreadsheet and
		// credentials, so a team writes them once.
		if s.Type == "sheetsource" {
			sheet := c.Store.Spreadsheet
			if c.Store.Type == "sqlite" {
				sheet = c.Store.ViewSpreadsheet
			}
			if sheet != "" {
				e["spreadsheet"] = sheet
			}
			if c.Store.Credentials != "" {
				e["credentials"] = c.Store.Credentials
			}
		}
		s.Block = deepCopy(e).(map[string]any)
		c.Sources = append(c.Sources, s)
	}
	return p.err
}

func (p *parser) enrich(c *Config, raw map[string]any) error {
	v, has := raw["enrich"]
	if !has {
		return nil
	}
	e, ok := v.(map[string]any)
	if !ok {
		return errors.New("`enrich` must be a mapping")
	}
	c.Enrich = &Enrich{
		Type:             p.str(e, "type", "enrich.type", ""),
		MaxAge:           p.duration(e, "max_age", "enrich.max_age", DefaultEnrichMaxAge),
		MaxLookupsPerRun: p.nonNegativeInt(e, "max_lookups_per_run", "enrich.max_lookups_per_run", DefaultLookupsPerRun),
		MaxLookupsPerDay: p.nonNegativeInt(e, "max_lookups_per_day", "enrich.max_lookups_per_day", DefaultLookupsPerDay),
	}
	c.Enrich.Block = deepCopy(e).(map[string]any)
	return p.err
}

func (p *parser) receiver(c *Config, raw map[string]any, getenv func(string) string) error {
	r := p.block(raw, "receiver", receiverKeys)
	c.Receiver.PublicURL = p.str(r, "public_url", "receiver.public_url", "")
	c.Receiver.VisitEvents = p.strList(r, "visit_events", "receiver.visit_events")
	r["visit_events"] = toAnyList(c.Receiver.VisitEvents)

	port := DefaultReceiverPort
	if env := getenv("PORT"); env != "" {
		n, err := strconv.Atoi(env)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("$PORT %q is not a port number", env)
		}
		port = n
	}
	if _, has := r["port"]; has {
		port = p.positiveInt(r, "port", "receiver.port", port)
		if port > 65535 && p.err == nil {
			p.fail("`receiver.port` %d is not a port number", port)
		}
	}
	c.Receiver.Port = port
	r["port"] = port
	return p.err
}

func (p *parser) sinks(c *Config, raw map[string]any) error {
	c.Sinks = map[string]api.Config{}
	v, has := raw["sinks"]
	if !has || v == nil {
		raw["sinks"] = map[string]any{}
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return errors.New("`sinks` must be a mapping of sink type to its block")
	}
	for typ, b := range m {
		block, ok := b.(map[string]any)
		if b == nil {
			block, ok = map[string]any{}, true
			m[typ] = block
		}
		if !ok {
			return fmt.Errorf("`sinks.%s` must be a mapping", typ)
		}
		if typ == "hubspot" {
			block["property_prefix"] = p.str(block, "property_prefix", "sinks.hubspot.property_prefix", DefaultPropertyPrefix)
		}
		c.Sinks[typ] = deepCopy(block).(map[string]any)
	}
	return p.err
}

func (p *parser) replyLabels(c *Config, raw map[string]any) {
	c.ReplyLabels = map[string]string{}
	v, has := raw["reply_labels"]
	if !has || v == nil {
		raw["reply_labels"] = map[string]any{}
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		p.fail("`reply_labels` must be a mapping of label to status")
		return
	}
	for label, val := range m {
		if label == "unsubscribe" {
			p.fail("`reply_labels.unsubscribe` cannot be overridden: an unsubscribe always opts the person out")
			return
		}
		s, ok := val.(string)
		if !ok || !replyLabelValues[s] {
			p.fail("`reply_labels.%s` must be one of replied_positive, replied_negative, replied_neutral, replied_unlabelled or none", label)
			return
		}
		c.ReplyLabels[label] = s
	}
}

// parser keeps the first error so the field readers stay one line each.
type parser struct {
	dir string
	err error
}

func (p *parser) fail(format string, args ...any) {
	if p.err == nil {
		p.err = fmt.Errorf(format, args...)
	}
}

// path resolves a relative path against the config folder.
func (p *parser) path(s string) string {
	if s == "" || filepath.IsAbs(s) {
		return s
	}
	return filepath.Join(p.dir, s)
}

// block returns the engine sub-block m[key], creating it when absent and
// rejecting keys outside allowed.
func (p *parser) block(m map[string]any, key string, allowed map[string]bool) map[string]any {
	v, has := m[key]
	if !has || v == nil {
		b := map[string]any{}
		m[key] = b
		return b
	}
	b, ok := v.(map[string]any)
	if !ok {
		p.fail("`%s` must be a mapping", key)
		b = map[string]any{}
		m[key] = b
		return b
	}
	if err := unknownKeys(b, allowed, key+"."); err != nil && p.err == nil {
		p.err = err
	}
	return b
}

// str reads a text value, writing def into m when the key is absent.
func (p *parser) str(m map[string]any, key, name, def string) string {
	v, has := m[key]
	if !has || v == nil {
		if def != "" {
			m[key] = def
		}
		return def
	}
	switch s := v.(type) {
	case string:
		return s
	case int, float64, bool:
		// A plain YAML scalar like a numeric spreadsheet id is still text here.
		return fmt.Sprint(s)
	}
	p.fail("`%s` must be text", name)
	return def
}

func (p *parser) boolean(m map[string]any, key, name string) bool {
	v, has := m[key]
	if !has || v == nil {
		m[key] = false
		return false
	}
	b, ok := v.(bool)
	if !ok {
		p.fail("`%s` must be true or false", name)
	}
	return b
}

func (p *parser) duration(m map[string]any, key, name, def string) time.Duration {
	text := def
	if v, has := m[key]; has && v != nil {
		s, ok := v.(string)
		if !ok {
			p.fail("`%s` must be a duration like 15m or 30d", name)
			return 0
		}
		text = s
	}
	m[key] = text
	d, err := ParseDuration(text)
	if err != nil {
		p.fail("`%s`: %v", name, err)
	}
	return d
}

func (p *parser) nonNegativeInt(m map[string]any, key, name string, def int) int {
	v, has := m[key]
	if !has || v == nil {
		m[key] = def
		return def
	}
	n, ok := v.(int)
	if !ok || n < 0 {
		p.fail("`%s` must be a whole number, zero or more", name)
		return def
	}
	return n
}

func (p *parser) positiveInt(m map[string]any, key, name string, def int) int {
	n := p.nonNegativeInt(m, key, name, def)
	if n == 0 {
		p.fail("`%s` must be more than zero", name)
	}
	return n
}

func (p *parser) strList(m map[string]any, key, name string) []string {
	v, has := m[key]
	if !has || v == nil {
		return []string{}
	}
	list, ok := v.([]any)
	if !ok {
		p.fail("`%s` must be a list", name)
		return []string{}
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			p.fail("`%s` must be a list of text", name)
			return []string{}
		}
		out = append(out, s)
	}
	return out
}

func unknownKeys(m map[string]any, allowed map[string]bool, prefix string) error {
	var bad []string
	for k := range m {
		if !allowed[k] {
			bad = append(bad, prefix+k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("unknown key %q", strings.Join(bad, `", "`))
}

func set(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func toAnyList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = deepCopy(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	}
	return v
}
