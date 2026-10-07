package config

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// SaveBudget is how long a run may save after its deadline. The Cloud Run job's
// task timeout is the deadline plus this.
const SaveBudget = 90 * time.Second

// AdapterKeyVariables names the key variable each built-in vendor adapter
// reads. Whoever changes an adapter's key variable keeps its entry here. The
// receiver's secret is the receiver-secret check's, not an adapter key. Stores
// sign in through Google's standard credentials and need none.
var AdapterKeyVariables = map[string]string{
	"apollo":  "APOLLO_API_KEY",
	"hubspot": "HUBSPOT_TOKEN",
}

// KeyVariables returns the key variables this configuration's adapters need,
// each with the config places that need it (`enrich`, `sinks.hubspot`,
// `sources.<id>`), sorted.
func (c *Config) KeyVariables() map[string][]string {
	needs := map[string][]string{}
	need := func(typ, where string) {
		if v, ok := AdapterKeyVariables[typ]; ok {
			needs[v] = append(needs[v], where)
		}
	}
	if c.Enrich != nil {
		need(c.Enrich.Type, "enrich")
	}
	for _, src := range c.Sources {
		need(src.Type, "sources."+src.ID)
	}
	for typ := range c.Sinks {
		need(typ, "sinks."+typ)
	}
	for _, users := range needs {
		sort.Strings(users)
	}
	return needs
}

// MaxBundleBytes is Secret Manager's limit on one secret version.
const MaxBundleBytes = 64 * 1024

// MakeBundle builds the hosted bundle: one YAML document with the
// keys `config` and `rubric`, each holding that file's text, which Load reads
// back as the same pair.
func MakeBundle(configText, rubricText []byte) ([]byte, error) {
	if len(configText) == 0 || len(rubricText) == 0 {
		return nil, errors.New("the bundle needs both leadscore.yml and the rubric")
	}
	doc := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, kv := range [][2]string{{"config", string(configText)}, {"rubric", string(rubricText)}} {
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[0]},
			// A literal block keeps each file readable in the console.
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[1], Style: yaml.LiteralStyle})
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("building the bundle: %w", err)
	}
	if len(out) > MaxBundleBytes {
		return nil, fmt.Errorf("leadscore.yml and the rubric together are %d bytes; Secret Manager holds at most %d", len(out), MaxBundleBytes)
	}
	// The bundle must read back as exactly these two files.
	cfg, rubric, isBundle, err := splitBundle(out, "bundle")
	if err != nil || !isBundle || string(cfg) != string(configText) || string(rubric) != string(rubricText) {
		return nil, errors.New("building the bundle: it does not read back as the same two files")
	}
	return out, nil
}
