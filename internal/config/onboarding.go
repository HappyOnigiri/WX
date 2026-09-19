package config

import (
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

func normalizeOnboardingRelative(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("repository relative path is required")
	}
	if clean := filepath.Clean(value); clean == "." {
		return clean, nil
	}
	return NormalizeRepositoryRelative(value)
}

// migrateLegacyWorkspaceOnboarding は旧 onboarding map を、root 記録と repository membership の記録へ移す。
// 読み込み時だけ旧形を受け付け、marshal は Config の新しい型から新形だけを出力する。
func migrateLegacyWorkspaceOnboarding(doc *yaml.Node) (bool, error) {
	if len(doc.Content) == 0 {
		return false, nil
	}
	workspaces := mappingValue(doc.Content[0], "workspaces")
	if workspaces == nil || workspaces.Kind != yaml.MappingNode {
		return false, nil
	}
	migrated := false
	for i := 0; i+1 < len(workspaces.Content); i += 2 {
		root, workspace := workspaces.Content[i].Value, workspaces.Content[i+1]
		onboarding := mappingValue(workspace, "onboarding")
		legacy, err := legacyOnboardingMapping(root, onboarding)
		if err != nil {
			return false, err
		}
		if !legacy {
			continue
		}
		if err := migrateLegacyOnboardingRecords(root, workspace, onboarding); err != nil {
			return false, err
		}
		migrated = true
	}
	return migrated, nil
}

func legacyOnboardingMapping(root string, onboarding *yaml.Node) (bool, error) {
	if onboarding == nil || onboarding.Kind != yaml.MappingNode {
		return false, nil
	}
	newFields, legacyFields := false, false
	for i := 0; i+1 < len(onboarding.Content); i += 2 {
		key, value := onboarding.Content[i].Value, onboarding.Content[i+1]
		switch {
		case (key == "checked_at" || key == "declined_at") && value.Kind != yaml.MappingNode:
			newFields = true
		case value.Kind == yaml.MappingNode:
			legacyFields = true
		default:
			// scalar の未知キーは新形式の typo として残し、通常の unknown key 診断へ渡す。
			newFields = true
		}
	}
	if newFields && legacyFields {
		return false, fmt.Errorf("workspaces.%s.onboarding mixes the current record with legacy repository paths", root)
	}
	return legacyFields, nil
}

func migrateLegacyOnboardingRecords(root string, workspace, onboarding *yaml.Node) error {
	var rootRecord *yaml.Node
	for i := 0; i+1 < len(onboarding.Content); i += 2 {
		relativePath, record := onboarding.Content[i].Value, onboarding.Content[i+1]
		clean, err := normalizeOnboardingRelative(relativePath)
		if err != nil {
			return fmt.Errorf("workspaces.%s.onboarding.%s must be a workspace-relative path", root, relativePath)
		}
		if clean == "." {
			rootRecord = record
			continue
		}
		if err := moveLegacyMemberOnboarding(root, workspace, clean, record); err != nil {
			return err
		}
	}
	if rootRecord != nil {
		setMappingValue(workspace, "onboarding", rootRecord)
	} else {
		removeMappingKey(workspace, "onboarding")
	}
	return nil
}

func moveLegacyMemberOnboarding(root string, workspace *yaml.Node, relativePath string, record *yaml.Node) error {
	repositories := mappingValue(workspace, "repositories")
	if repositories == nil {
		repositories = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(workspace, "repositories", repositories)
	}
	if repositories.Kind != yaml.MappingNode {
		return fmt.Errorf("workspaces.%s.repositories must be a mapping", root)
	}
	repository := mappingValue(repositories, relativePath)
	if repository == nil {
		repository = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(repositories, relativePath, repository)
	}
	if repository.Kind != yaml.MappingNode {
		return fmt.Errorf("workspaces.%s.repositories.%s must be a mapping", root, relativePath)
	}
	if mappingValue(repository, "onboarding") != nil {
		return fmt.Errorf("workspaces.%s repository %s has onboarding records in both legacy and current locations", root, relativePath)
	}
	setMappingValue(repository, "onboarding", record)
	return nil
}

func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, scalarKeyNode(key), value)
}
