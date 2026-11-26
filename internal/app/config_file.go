package app

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultConfigFilePath is the built-in fallback for CONFIG_FILE.
	DefaultConfigFilePath = "./data/config.yaml"
)

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
		filePath = DefaultConfigFilePath
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

func hasAdminWildcard(raw string) bool {
	for _, item := range strings.Split(raw, ",") {
		if strings.TrimSpace(item) == "*" {
			return true
		}
	}
	return false
}

func parseAdminIDs(raw string) (ids map[int64]bool, wildcard bool) {
	if hasAdminWildcard(raw) {
		return nil, true
	}
	return parseIDList(raw), false
}

func loadYAMLConfigValues(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	doc, err := configYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	return yamlConfigValues(doc), nil
}

func yamlConfigValues(doc *yaml.Node) map[string]string {
	root := doc.Content[0]
	values := make(map[string]string, len(root.Content)/2)
	for i := 0; i+1 < len(root.Content); i += 2 {
		keyNode := root.Content[i]
		valueNode := root.Content[i+1]
		values[strings.TrimSpace(keyNode.Value)] = yamlNodeToConfigString(valueNode)
	}
	return values
}

func configYAMLDocument(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the YAML top level must be a mapping")
	}
	return &doc, nil
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
		return DefaultConfigFilePath
	}
	return path
}

func saveConfigFile(cfg Config) error {
	if cfg.AdminWildcard {
		return fmt.Errorf("ADMIN_ID does not accept a wildcard; configure explicit Telegram user IDs")
	}
	path := effectiveConfigFilePath(cfg)

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
	data, err := encodeConfigYAML(doc)
	if err != nil {
		return err
	}
	return replaceConfigFile(path, data)
}

func encodeConfigYAML(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, fmt.Errorf("failed to encode YAML: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("failed to close the YAML encoder: %w", err)
	}

	return buf.Bytes(), nil
}

func replaceConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create the config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("failed to create the temporary config file: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return fmt.Errorf("failed to write the temporary config file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("failed to replace the config file: %w", err)
	}
	return nil
}

func updateConfigOption(doc *yaml.Node, option configOption, next Config) {
	root := doc.Content[0]
	value := configStringToYAMLNode(option.GetValue(next))
	for i := len(root.Content) - 2; i >= 0; i -= 2 {
		if strings.TrimSpace(root.Content[i].Value) == option.EnvKey {
			old := root.Content[i+1]
			value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
			root.Content[i+1] = value
			preserveConfigAliases(doc, old)
			return
		}
	}
	root.Content = append(root.Content, &yaml.Node{
		Kind: yaml.ScalarNode, Tag: "!!str", Value: option.EnvKey, HeadComment: option.Desc,
	}, value)
}

// Move any referenced anchors from the replaced value to their first
// remaining use, so other keys keep referring to the original values.
func preserveConfigAliases(doc, removed *yaml.Node) {
	placed := make(map[*yaml.Node]bool)
	var collect func(*yaml.Node)
	collect = func(node *yaml.Node) {
		if node.Anchor != "" {
			placed[node] = false
		}
		for _, child := range node.Content {
			collect(child)
		}
	}
	collect(removed)
	if len(placed) == 0 {
		return
	}
	var restore func(*yaml.Node) *yaml.Node
	restore = func(node *yaml.Node) *yaml.Node {
		if node.Kind == yaml.AliasNode {
			if restored, ok := placed[node.Alias]; !ok || restored {
				return node
			}
			node = node.Alias
		}
		if restored, ok := placed[node]; ok {
			if restored {
				return &yaml.Node{Kind: yaml.AliasNode, Value: node.Anchor, Alias: node}
			}
			placed[node] = true
		}
		for i, child := range node.Content {
			node.Content[i] = restore(child)
		}
		return node
	}
	restore(doc)
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
		if envKey == "ADMIN_ID" {
			return "\n\n⚠️ The environment still controls the active administrator IDs. This edit updates the YAML value."
		}
		return "\n\n⚠️ This key is also overridden by an environment variable, so after restart the env value will still win."
	}
	return ""
}

// The caller holds runtimeConfigMu across the authorization check and the save.
func (b *Bot) applyAndPersistConfigLocked(current, next Config, option configOption) error {
	path := effectiveConfigFilePath(current)
	next.ConfigFilePath = path
	if next.AdminWildcard {
		return fmt.Errorf("ADMIN_ID does not accept a wildcard; configure explicit Telegram user IDs")
	}
	yamlValue := next
	if raw := os.Getenv("ADMIN_ID"); raw != "" {
		next.AdminIDs, next.AdminWildcard = parseAdminIDs(raw)
	}
	if next.AdminWildcard {
		return fmt.Errorf("ADMIN_ID does not accept a wildcard; configure explicit Telegram user IDs")
	}
	// Environment-supplied secrets and edits under their override stay in memory.
	if option.Sensitive && strings.TrimSpace(os.Getenv(option.EnvKey)) != "" {
		return b.applyRuntimeConfigLocked(next)
	}

	original, err := os.ReadFile(path)
	missing := os.IsNotExist(err)
	if err != nil && !missing {
		return fmt.Errorf("failed to read the config file: %w", err)
	}
	doc, err := configYAMLDocument(original)
	if err != nil {
		return err
	}
	before := yamlConfigValues(doc)
	updateConfigOption(doc, option, yamlValue)
	encoded, err := encodeConfigYAML(doc)
	if err != nil {
		return err
	}
	savedDoc, err := configYAMLDocument(encoded)
	if err != nil {
		return fmt.Errorf("the edit cannot preserve the existing YAML aliases")
	}
	// Compare the loader's values even for keys currently overridden by the
	// environment. A migrated anchor must not activate a previously ignored alias.
	after := yamlConfigValues(savedDoc)
	delete(before, option.EnvKey)
	delete(after, option.EnvKey)
	if !maps.Equal(before, after) {
		return fmt.Errorf("the edit would change another config value through a YAML alias")
	}
	if err := replaceConfigFile(path, encoded); err != nil {
		return fmt.Errorf("failed to write the config file: %w", err)
	}
	if err := b.applyRuntimeConfigLocked(next); err != nil {
		var restoreErr error
		if missing {
			restoreErr = os.Remove(path)
		} else {
			restoreErr = replaceConfigFile(path, original)
		}
		if restoreErr != nil {
			return errors.Join(err, fmt.Errorf("failed to restore the original config file: %w", restoreErr))
		}
		return err
	}
	return nil
}
