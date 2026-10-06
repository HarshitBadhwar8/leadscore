package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrKeyNotSet is returned by Get for a key with no value and no default.
var ErrKeyNotSet = errors.New("key not set")

// Get returns the effective value of a dotted key (`store.type`,
// `hosting.project`, `sources.0.id`): the file's value, else its default, with
// relative paths resolved. A scalar prints as text; a mapping or list prints as
// YAML.
func (c *Config) Get(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty key")
	}
	var cur any = c.eff
	for _, part := range strings.Split(key, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[part]
			if !ok {
				return "", fmt.Errorf("%s: %w", key, ErrKeyNotSet)
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node) {
				return "", fmt.Errorf("%s: %w", key, ErrKeyNotSet)
			}
			cur = node[i]
		default:
			return "", fmt.Errorf("%s: %w", key, ErrKeyNotSet)
		}
	}
	switch v := cur.(type) {
	case nil:
		return "", fmt.Errorf("%s: %w", key, ErrKeyNotSet)
	case string:
		return v, nil
	case map[string]any, []any:
		var buf strings.Builder
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(v); err != nil {
			return "", err
		}
		if err := enc.Close(); err != nil {
			return "", err
		}
		return strings.TrimRight(buf.String(), "\n"), nil
	default:
		return fmt.Sprint(v), nil
	}
}

// SetHosting writes key=value pairs into the `hosting` block of the
// leadscore.yml at path, keeping its comments, and leaves every other key as it
// was. Only the section 3 hosting keys are accepted. The result must still load,
// and the file is replaced atomically, so a failed write never leaves half a file.
func SetHosting(path string, pairs []string) error {
	if len(pairs) == 0 {
		return errors.New("nothing to set: give one or more key=value pairs")
	}
	type kv struct{ k, v string }
	var sets []kv
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return fmt.Errorf("%q is not key=value", pair)
		}
		if !hostingKeys[k] {
			return fmt.Errorf("unknown hosting key %q (allowed: %s)", k, strings.Join(HostingKeys, ", "))
		}
		sets = append(sets, kv{k, v})
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if _, _, isBundle, err := splitBundle(data, path); err != nil {
		return err
	} else if isBundle {
		return fmt.Errorf("%s is a hosted bundle; set hosting in leadscore.yml and run `leadscore config push`", path)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a YAML mapping at the top", path)
	}
	root := doc.Content[0]

	hosting := mappingValue(root, "hosting")
	if hosting == nil {
		hosting = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "hosting"}, hosting)
	} else if hosting.Kind != yaml.MappingNode {
		// `hosting:` with no value, or a scalar: replace the value, keep its comments.
		*hosting = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map",
			HeadComment: hosting.HeadComment, LineComment: hosting.LineComment, FootComment: hosting.FootComment}
	}
	for _, s := range sets {
		if v := mappingValue(hosting, s.k); v != nil {
			// Change the scalar in place so its line comment stays.
			v.Kind, v.Tag, v.Value, v.Style, v.Content = yaml.ScalarNode, "!!str", s.v, 0, nil
			continue
		}
		hosting.Content = append(hosting.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.k},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.v})
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("writing YAML: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("writing YAML: %w", err)
	}
	out := []byte(buf.String())
	if _, err := parse(out, filepath.Dir(path), func(string) string { return "" }); err != nil {
		return fmt.Errorf("%s would no longer load: %w", path, err)
	}
	return writeAtomic(path, out)
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	// Write through a symlink rather than replacing the link with a file.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".leadscore-*.yml")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
