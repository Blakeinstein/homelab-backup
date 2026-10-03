// Package yamledit provides in-place mutation helpers for the backup
// services yaml: the document is round-tripped through yaml.Node so that
// existing comments, key order and formatting survive edits. Raw values
// (including ${VAR} references) are never expanded.
package yamledit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load reads a yaml document and returns it as a Document node.
func Load(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes yaml bytes into a DocumentNode.
func Parse(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, fmt.Errorf("yaml document is empty")
	}
	return &doc, nil
}

// Root returns the top-level mapping of a document node.
func M(doc *yaml.Node) (*yaml.Node, error) {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("not a yaml document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml top level must be a mapping")
	}
	return root, nil
}

// Marshal encodes the document with stable 2-space block indentation.
func Marshal(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		enc.Close()
		return nil, err
	}
	enc.Close()
	return buf.Bytes(), nil
}

// Write marshals and stores the document (temp file + rename).
func Write(doc *yaml.Node, path string) error {
	b, err := Marshal(doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// ---- mapping helpers -------------------------------------------------------

func pairIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func keyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

// Map returns the mapping value for key (nil when absent and create=false).
func Map(m *yaml.Node, key string, create bool) (*yaml.Node, error) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml node is not a mapping")
	}
	if i := pairIndex(m, key); i >= 0 {
		v := m.Content[i+1]
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("yaml key %q is not a mapping", key)
		}
		return v, nil
	}
	if !create {
		return nil, fmt.Errorf("yaml key %q not found", key)
	}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, keyNode(key), v)
	return v, nil
}

// Seq returns the sequence value for key (nil when absent and create=false).
func Seq(m *yaml.Node, key string, create bool) (*yaml.Node, error) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml node is not a mapping")
	}
	if i := pairIndex(m, key); i >= 0 {
		v := m.Content[i+1]
		if v.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("yaml key %q is not a list", key)
		}
		return v, nil
	}
	if !create {
		return nil, fmt.Errorf("yaml key %q not found", key)
	}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, keyNode(key), v)
	return v, nil
}

// Preserve the existing value slot when replacing a subtree (keeps comments
// on the key node): replaces the value node in place.
func SetValue(m *yaml.Node, key string, val *yaml.Node) {
	if i := pairIndex(m, key); i >= 0 {
		m.Content[i+1] = val
		return
	}
	m.Content = append(m.Content, keyNode(key), val)
}

// SetString sets key to a plain string scalar (creating the key when missing).
func SetString(m *yaml.Node, key, val string) {
	if i := pairIndex(m, key); i >= 0 {
		v := m.Content[i+1]
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Style = 0
		v.Value = val
		return
	}
	m.Content = append(m.Content, keyNode(key), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val})
}

// SetInt sets key to an integer scalar.
func SetInt(m *yaml.Node, key string, val int) {
	s := fmt.Sprintf("%d", val)
	if i := pairIndex(m, key); i >= 0 {
		v := m.Content[i+1]
		v.Kind = yaml.ScalarNode
		v.Tag = "!!int"
		v.Style = 0
		v.Value = s
		return
	}
	m.Content = append(m.Content, keyNode(key), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: s})
}

// SetBool sets key to a bool scalar.
func SetBool(m *yaml.Node, key string, val bool) {
	s := "false"
	if val {
		s = "true"
	}
	if i := pairIndex(m, key); i >= 0 {
		v := m.Content[i+1]
		v.Kind = yaml.ScalarNode
		v.Tag = "!!bool"
		v.Style = 0
		v.Value = s
		return
	}
	m.Content = append(m.Content, keyNode(key), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: s})
}

// SetStringSeq sets key to a list of strings; an empty list removes the key.
// (Existing node comments are dropped — documented tradeoff.)
func SetStringSeq(m *yaml.Node, key string, vals []string) {
	if len(vals) == 0 {
		Remove(m, key)
		return
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range vals {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	if i := pairIndex(m, key); i >= 0 {
		m.Content[i+1] = seq
		return
	}
	m.Content = append(m.Content, keyNode(key), seq)
}

// Remove deletes key (and its value) from the mapping if present.
func Remove(m *yaml.Node, key string) {
	if i := pairIndex(m, key); i >= 0 {
		m.Content = append(m.Content[:i], m.Content[i+2:]...)
	}
}

// Get returns the scalar string value for key ("" when missing).
func Get(m *yaml.Node, key string) string {
	if i := pairIndex(m, key); i >= 0 {
		v := m.Content[i+1]
		if v.Kind == yaml.ScalarNode {
			return v.Value
		}
	}
	return ""
}

// GetInt parses key as an integer (0 when missing/invalid).
func GetInt(m *yaml.Node, key string) (int, bool) {
	i := pairIndex(m, key)
	if i < 0 {
		return 0, false
	}
	v := m.Content[i+1]
	if v.Kind != yaml.ScalarNode {
		return 0, false
	}
	n := 0
	if _, err := fmt.Sscanf(v.Value, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// GetBool parses key as a bool (false when missing).
func GetBool(m *yaml.Node, key string) bool {
	return Get(m, key) == "true"
}

// GetStringSeq returns the string elements of key (nil when missing).
func GetStrings(m *yaml.Node, key string) []string {
	i := pairIndex(m, key)
	if i < 0 || m.Content[i+1].Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, el := range m.Content[i+1].Content {
		out = append(out, el.Value)
	}
	return out
}

// Keys returns the plain keys of a mapping (in document order).
func Keys(m *yaml.Node) []string {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(m.Content); i += 2 {
		out = append(out, m.Content[i].Value)
	}
	return out
}
