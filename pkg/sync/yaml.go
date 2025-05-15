package sync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	vault "github.com/sosedoff/ansible-vault-go"
	"gopkg.in/yaml.v3"
)

// LoadVariablesFile loads vars yaml file from path.
func LoadVariablesFile(path, vaultPassword string, isVault bool) (map[string]any, []string, error) {
	var data map[string]any
	var rawData []byte
	var debugMessages []string
	var err error

	cleanPath := filepath.Clean(path)
	if isVault {
		sourceVault, errDecrypt := vault.DecryptFile(cleanPath, vaultPassword)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, nil, fmt.Errorf("error decrypting vault %s, password is blank", cleanPath)
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, nil, fmt.Errorf("error decrypting vault %s, invalid secret format", cleanPath)
			} else if errDecrypt.Error() == invalidPasswordErrText {
				return data, nil, fmt.Errorf("invalid password for vault '%s'", cleanPath)
			}

			return data, nil, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData, err = os.ReadFile(cleanPath)
		if err != nil {
			return data, nil, err
		}
	}

	err = yaml.Unmarshal(rawData, &data)
	if err != nil {
		if !strings.Contains(err.Error(), "already defined at line") {
			return data, nil, err
		}

		var duplicates map[string]bool
		data, duplicates, debugMessages, err = UnmarshallFixDuplicates(rawData)
		if err != nil {
			return data, nil, err
		}

		for d := range duplicates {
			debugMessages = append(debugMessages, fmt.Sprintf("duplicate found for variable %s in file %s, using last occurrence", d, path))
		}
	}

	return data, debugMessages, err
}

// LoadVariablesFileFromBytes loads vars yaml file from bytes input.
func LoadVariablesFileFromBytes(input []byte, path string, vaultPassword string, isVault bool) (map[string]any, []string, error) {
	var data map[string]any
	var rawData []byte
	var debugMessages []string
	var err error

	if isVault {
		sourceVault, errDecrypt := vault.Decrypt(string(input), vaultPassword)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, nil, fmt.Errorf("error decrypting vaults, password is blank")
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, nil, fmt.Errorf("error decrypting vault, invalid secret format")
			} else if errDecrypt.Error() == invalidPasswordErrText {
				return data, nil, fmt.Errorf("invalid password for vault")
			}

			return data, nil, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData = input
	}

	err = yaml.Unmarshal(rawData, &data)
	if err != nil {
		if !strings.Contains(err.Error(), "already defined at line") {
			return data, nil, err
		}

		var duplicates map[string]bool
		data, duplicates, debugMessages, err = UnmarshallFixDuplicates(rawData)
		if err != nil {
			return data, debugMessages, err
		}

		for d := range duplicates {
			debugMessages = append(debugMessages, fmt.Sprintf("duplicate found for variable %s in file %s, using last occurrence", d, path))
		}
	}

	return data, debugMessages, err
}

// LoadYamlFileFromBytes loads yaml file from bytes input.
func LoadYamlFileFromBytes(input []byte) (map[string]any, error) {
	var data map[string]any
	var rawData []byte
	var err error

	rawData = input
	err = yaml.Unmarshal(rawData, &data)
	return data, err
}

// UnmarshallFixDuplicates handles duplicated values in yaml instead throwing error.
func UnmarshallFixDuplicates(data []byte) (map[string]any, map[string]bool, []string, error) {
	var debugMessages []string
	reader := bytes.NewReader(data)
	decoder := yaml.NewDecoder(reader)

	result := make(map[string]any)
	duplicates := make(map[string]bool)
	for {
		var rootNode yaml.Node
		err := decoder.Decode(&rootNode)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, nil, debugMessages, fmt.Errorf("error parsing YAML document: %w", err)
		}

		// Parse each document and merge
		res, err := recursiveParse(&rootNode, duplicates, &debugMessages)
		if err != nil {
			return nil, nil, debugMessages, err
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

	return result, duplicates, debugMessages, nil
}

// Recursive parsing function to handle YAML data with duplicate keys.
func recursiveParse(node *yaml.Node, duplicates map[string]bool, debug *[]string) (any, error) {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) > 0 {
			return recursiveParse(node.Content[0], duplicates, debug)
		}
		return nil, nil

	case yaml.AliasNode:
		return recursiveParse(node.Alias, duplicates, debug)

	case yaml.ScalarNode:
		var value any
		if err := node.Decode(&value); err != nil {
			*debug = append(*debug, fmt.Sprintf("Failed to decode scalar at line %d, column %d: %s", node.Line, node.Column, err.Error()))
			return node.Value, nil
		}
		return value, nil

	case yaml.SequenceNode:
		var result []any
		for _, n := range node.Content {
			value, err := recursiveParse(n, duplicates, debug)
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
			value, err := recursiveParse(valueNode, duplicates, debug)
			if err != nil {
				return result, err
			}

			// Manage duplicate keys
			if _, found := result[key]; found {
				duplicates[key] = true
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
