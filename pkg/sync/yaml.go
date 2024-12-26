package sync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/launchrctl/launchr"
	vault "github.com/sosedoff/ansible-vault-go"
	"gopkg.in/yaml.v3"
)

// LoadVariablesFile loads vars yaml file from path.
func LoadVariablesFile(path, vaultPassword string, isVault bool) (map[string]any, error) {
	var data map[string]any
	var rawData []byte
	var err error

	cleanPath := filepath.Clean(path)
	if isVault {
		sourceVault, errDecrypt := vault.DecryptFile(cleanPath, vaultPassword)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, fmt.Errorf("error decrypting vault %s, password is blank", cleanPath)
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, fmt.Errorf("error decrypting vault %s, invalid secret format", cleanPath)
			} else if errDecrypt.Error() == invalidPasswordErrText {
				return data, fmt.Errorf("invalid password for vault '%s'", cleanPath)
			}

			return data, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData, err = os.ReadFile(cleanPath)
		if err != nil {
			return data, err
		}
	}

	err = yaml.Unmarshal(rawData, &data)
	if err != nil {
		if !strings.Contains(err.Error(), "already defined at line") {
			return data, err
		}

		var duplicates map[string]bool
		data, duplicates, err = UnmarshallFixDuplicates(rawData)
		if err != nil {
			return data, err
		}

		for d := range duplicates {
			launchr.Log().Debug(fmt.Sprintf("duplicate found for variable %s in file %s, using last occurrence", d, path))
		}
	}

	return data, err
}

// LoadVariablesFileFromBytes loads vars yaml file from bytes input.
func LoadVariablesFileFromBytes(input []byte, path string, vaultPassword string, isVault bool) (map[string]any, error) {
	var data map[string]any
	var rawData []byte
	var err error

	if isVault {
		sourceVault, errDecrypt := vault.Decrypt(string(input), vaultPassword)
		if errDecrypt != nil {
			if errors.Is(errDecrypt, vault.ErrEmptyPassword) {
				return data, fmt.Errorf("error decrypting vaults, password is blank")
			} else if errors.Is(errDecrypt, vault.ErrInvalidFormat) {
				return data, fmt.Errorf("error decrypting vault, invalid secret format")
			} else if errDecrypt.Error() == invalidPasswordErrText {
				return data, fmt.Errorf("invalid password for vault")
			}

			return data, errDecrypt
		}
		rawData = []byte(sourceVault)
	} else {
		rawData = input
	}

	err = yaml.Unmarshal(rawData, &data)
	if err != nil {
		if !strings.Contains(err.Error(), "already defined at line") {
			return data, err
		}

		var duplicates map[string]bool
		data, duplicates, err = UnmarshallFixDuplicates(rawData)
		if err != nil {
			return data, err
		}

		for d := range duplicates {
			launchr.Log().Debug(fmt.Sprintf("duplicate found for variable %s in file %s, using last occurrence", d, path))
		}
	}

	return data, err
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
func UnmarshallFixDuplicates(data []byte) (map[string]any, map[string]bool, error) {
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
			return nil, nil, fmt.Errorf("error parsing YAML document: %w", err)
		}

		// Parse each document and merge
		res, err := recursiveParse(&rootNode, duplicates)
		if err != nil {
			return nil, nil, err
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

	return result, duplicates, nil
}

// Recursive parsing function to handle YAML data with duplicate keys.
func recursiveParse(node *yaml.Node, duplicates map[string]bool) (any, error) {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) > 0 {
			return recursiveParse(node.Content[0], duplicates)
		}
		return nil, nil

	case yaml.AliasNode:
		return recursiveParse(node.Alias, duplicates)

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
			value, err := recursiveParse(n, duplicates)
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
			value, err := recursiveParse(valueNode, duplicates)
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
