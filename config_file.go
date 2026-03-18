package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

const defaultConfigFilePath = "config.yaml"

type configValueSource struct {
	filePath string
	values   map[string]string
}

func loadConfigValues() configValueSource {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, reading from environment")
	}

	filePath := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	if filePath == "" {
		filePath = defaultConfigFilePath
	}

	values, err := loadYAMLConfigValues(filePath)
	if err != nil {
		log.Printf("Warning: failed to load config file %s: %v", filePath, err)
	}

	return configValueSource{
		filePath: filePath,
		values:   values,
	}
}

func (s configValueSource) Get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if s.values != nil {
		if v := s.values[key]; v != "" {
			return v
		}
	}
	return fallback
}

func loadYAMLConfigValues(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the YAML top level must be a mapping")
	}

	values := make(map[string]string, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valueNode := root.Content[i+1]
		values[strings.TrimSpace(keyNode.Value)] = yamlNodeToConfigString(valueNode)
	}
	return values, nil
}

func yamlNodeToConfigString(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Value
	case yaml.SequenceNode:
		parts := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			parts = append(parts, yamlNodeToConfigString(child))
		}
		return strings.Join(parts, ",")
	default:
		return ""
	}
}

func ensureConfigFileExists(cfg Config) error {
	path := effectiveConfigFilePath(cfg)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return saveConfigFile(cfg)
}

func effectiveConfigFilePath(cfg Config) string {
	path := strings.TrimSpace(cfg.ConfigFilePath)
	if path == "" {
		return defaultConfigFilePath
	}
	return path
}

func saveConfigFile(cfg Config) error {
	path := effectiveConfigFilePath(cfg)
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create the config directory: %w", err)
		}
	}

	doc := &yaml.Node{Kind: yaml.DocumentNode}
	root := &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
	}
	doc.Content = []*yaml.Node{root}
	doc.HeadComment = "Managed by llm_telebot.\nEnvironment variables override these values at startup."

	for _, option := range allConfigOptions() {
		key := &yaml.Node{
			Kind:        yaml.ScalarNode,
			Tag:         "!!str",
			Value:       option.EnvKey,
			HeadComment: option.Desc,
		}
		valueText := option.GetValue(cfg)
		if option.Sensitive && strings.TrimSpace(os.Getenv(option.EnvKey)) != "" {
			valueText = ""
		}
		value := configStringToYAMLNode(valueText)
		root.Content = append(root.Content, key, value)
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return fmt.Errorf("failed to encode YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("failed to close the YAML encoder: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to write the temporary config file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to replace the config file: %w", err)
	}
	return nil
}

func configStringToYAMLNode(raw string) *yaml.Node {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
	case strings.Contains(raw, "\n"):
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: raw, Style: yaml.LiteralStyle}
	}

	lower := strings.ToLower(raw)
	if lower == "true" || lower == "false" {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: lower}
	}
	if _, err := strconv.Atoi(raw); err == nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: raw}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: raw}
}

func configOverrideNotice(envKey string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return "\n\n⚠️ This key is also overridden by an environment variable, so after restart the env value will still win."
	}
	return ""
}

func (b *Bot) applyAndPersistConfig(next Config) error {
	current := b.currentConfig()
	next.ConfigFilePath = effectiveConfigFilePath(current)

	if err := saveConfigFile(next); err != nil {
		return fmt.Errorf("failed to write the config file: %w", err)
	}
	if err := b.applyRuntimeConfig(next); err != nil {
		_ = saveConfigFile(current)
		return err
	}
	return nil
}
