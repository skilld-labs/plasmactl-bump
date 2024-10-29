package sync

import (
	"bytes"
	"fmt"

	"github.com/launchrctl/launchr"
	"gopkg.in/yaml.v3"
)

// UnmarshallFixDuplicates handles duplicated values in yaml instead throwing error.
func UnmarshallFixDuplicates(data []byte) (map[string]any, error) {
	reader := bytes.NewReader(data)
	decoder := yaml.NewDecoder(reader)

	result := make(map[string]any)
	for {
		var rootNode yaml.Node
		err := decoder.Decode(&rootNode)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("error parsing YAML document: %w", err)
		}

		// Parse each document and merge
		res, err := recursiveParse(&rootNode)
		if err != nil {
			return nil, err
		}
		doc := res.(map[string]any)
		for k, v := range doc {
			if _, found := result[k]; found {
				//launchr.Log().Debug("overriding duplicated key in YAML", "key", k)
				result[k] = v
			} else {
				result[k] = v
			}
		}
	}

	return result, nil
}

// Recursive parsing function to handle YAML data with duplicate keys.
func recursiveParse(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) > 0 {
			return recursiveParse(node.Content[0])
		}
		return nil, nil

	case yaml.AliasNode:
		return recursiveParse(node.Alias)

	case yaml.ScalarNode:
		var value any
		if err := node.Decode(&value); err != nil {
			launchr.Term().Warning().Printfln("Failed to decode scalar at line %d, column %d: %v", node.Line, node.Column, err)
			return node.Value, nil
		}
		return value, nil

	case yaml.SequenceNode:
		var result []any
		for _, n := range node.Content {
			value, err := recursiveParse(n)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}

		return result, nil

	case yaml.MappingNode:
		// Handle mappings and detect duplicates
		result := make(map[string]any)
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]
			key := keyNode.Value
			value, err := recursiveParse(valueNode)
			if err != nil {
				return result, err
			}

			// Manage duplicate keys
			if _, found := result[key]; found {
				//launchr.Log().Debug("overriding duplicated key in YAML", "key", key)
				result[key] = value
			} else {
				result[key] = value
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unhandled YAML node kind at line %d, column %d: %v", node.Line, node.Column, node.Kind)
	}
}
