package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
)

// Workflow is an API-format graph: node id to {class_type, inputs}.
type Workflow struct {
	Graph map[string]any
}

// NewWorkflow wraps a graph.
func NewWorkflow(graph map[string]any) *Workflow { return &Workflow{Graph: graph} }

// WorkflowFromJSON parses an API-format graph. Numbers stay exact, so a
// 64-bit seed survives.
func WorkflowFromJSON(data []byte) (*Workflow, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var graph map[string]any
	if err := dec.Decode(&graph); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	if graph == nil {
		return nil, fmt.Errorf("parse workflow: not a JSON object")
	}
	return &Workflow{Graph: graph}, nil
}

// WorkflowFromFile reads an API-format graph from disk.
func WorkflowFromFile(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	return WorkflowFromJSON(data)
}

// SetInput sets a node input, creating the node and its inputs map when
// missing. An *Asset value is uploaded on submit and replaced with its
// core/ASSET reference.
func (w *Workflow) SetInput(nodeID, field string, value any) {
	if w.Graph == nil {
		w.Graph = map[string]any{}
	}
	node, ok := w.Graph[nodeID].(map[string]any)
	if !ok {
		node = map[string]any{}
		w.Graph[nodeID] = node
	}
	inputs, ok := node["inputs"].(map[string]any)
	if !ok {
		inputs = map[string]any{}
		node["inputs"] = inputs
	}
	inputs[field] = value
}

// isLink is a [source_node_id, output_index] input.
func isLink(v any) (string, int, bool) {
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return "", 0, false
	}
	src, ok := arr[0].(string)
	if !ok {
		return "", 0, false
	}
	switch n := arr[1].(type) {
	case float64:
		return src, int(n), true
	case int:
		return src, n, true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return "", 0, false
		}
		return src, int(f), true
	}
	return "", 0, false
}

// RemoveNode deletes a node. When the removed node had exactly one linked
// input, links that pointed at it are rewired to that input's source, so a
// pass-through node (an upscale, a preview) can be cut out; otherwise links to
// it are dropped.
func (w *Workflow) RemoveNode(nodeID string) {
	removed, ok := w.Graph[nodeID]
	if !ok {
		return
	}
	delete(w.Graph, nodeID)

	type link struct {
		node string
		out  int
	}
	var sources []link
	if m, ok := removed.(map[string]any); ok {
		if inputs, ok := m["inputs"].(map[string]any); ok {
			for _, v := range inputs {
				if src, out, ok := isLink(v); ok {
					sources = append(sources, link{src, out})
				}
			}
		}
	}
	for _, n := range w.Graph {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		inputs, ok := m["inputs"].(map[string]any)
		if !ok {
			continue
		}
		for key, v := range inputs {
			src, _, ok := isLink(v)
			if !ok || src != nodeID {
				continue
			}
			if len(sources) == 1 {
				if _, exists := w.Graph[sources[0].node]; exists {
					inputs[key] = []any{sources[0].node, sources[0].out}
					continue
				}
			}
			delete(inputs, key)
		}
	}
}

// uiKeys mark an editor export, which is not the API format.
var uiKeys = []string{"nodes", "links", "last_node_id"}

func looksLikeUIFormat(graph map[string]any) bool {
	for _, k := range uiKeys {
		if _, ok := graph[k]; !ok {
			return false
		}
	}
	return true
}

// materialize commits every asset handle in the graph and returns a copy
// with each replaced by its core/ASSET reference.
func (c *Client) materialize(ctx context.Context, graph map[string]any) (map[string]any, error) {
	out, err := c.substitute(ctx, graph)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

func (c *Client) substitute(ctx context.Context, v any) (any, error) {
	if out, ok, err := c.substituteTyped(ctx, v); ok || err != nil {
		return out, err
	}
	switch t := v.(type) {
	case *Asset:
		return t.Reference(ctx)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			s, err := c.substitute(ctx, child)
			if err != nil {
				return nil, err
			}
			out[k] = s
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			s, err := c.substitute(ctx, child)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
		return out, nil
	}
	return v, nil
}

// substituteTyped handles typed slices and string-keyed maps, such as
// []*Asset or map[string]*Asset, which the type switch does not see. Left
// alone they would marshal each *Asset to {} and send a broken graph.
func (c *Client) substituteTyped(ctx context.Context, v any) (any, bool, error) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if _, plain := v.([]any); plain || rv.Type().Elem().Kind() == reflect.Uint8 {
			return nil, false, nil
		}
		out := make([]any, rv.Len())
		for i := range out {
			s, err := c.substitute(ctx, rv.Index(i).Interface())
			if err != nil {
				return nil, true, err
			}
			out[i] = s
		}
		return out, true, nil
	case reflect.Map:
		if _, plain := v.(map[string]any); plain || rv.Type().Key().Kind() != reflect.String {
			return nil, false, nil
		}
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			s, err := c.substitute(ctx, iter.Value().Interface())
			if err != nil {
				return nil, true, err
			}
			out[iter.Key().String()] = s
		}
		return out, true, nil
	}
	return nil, false, nil
}
