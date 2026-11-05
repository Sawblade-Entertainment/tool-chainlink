package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type SourcemapNode struct {
	Name      string           `json:"name"`
	ClassName string           `json:"className"`
	FilePaths []string         `json:"filePaths,omitempty"`
	Children  []*SourcemapNode `json:"children,omitempty"`
}

func handleSourcemap(outputPath string) error {
	cfg := loadConfig()
	root, err := os.Getwd()
	if err != nil {
		return err
	}

	// Only names, classes and file paths are needed, so skip reading and
	// transforming script contents.
	b := NewBuilder(cfg, root)
	b.namesOnly = true
	b.populatePlace()

	// The root of the sourcemap is a virtual node representing the project.
	rootNode := &SourcemapNode{
		Name:      filepath.Base(root),
		ClassName: "DataModel",
		FilePaths: []string{"chain.toml"},
	}

	for _, inst := range b.top {
		if node := convertToSourcemapNode(inst); node != nil {
			rootNode.Children = append(rootNode.Children, node)
		}
	}

	data, err := json.MarshalIndent(rootNode, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal sourcemap: %w", err)
	}

	if outputPath != "" {
		if err := os.WriteFile(outputPath, data, 0644); err != nil {
			return fmt.Errorf("failed to write sourcemap to file: %w", err)
		}
	} else {
		fmt.Println(string(data))
	}
	return nil
}

// convertToSourcemapNode returns nil for embedded prefab items without a
// name, which luau-lsp could not resolve.
func convertToSourcemapNode(inst *Instance) *SourcemapNode {
	if inst.Name == "" && inst.RawXML != "" {
		return nil
	}
	node := &SourcemapNode{
		Name:      inst.Name,
		ClassName: inst.Class,
	}

	if inst.FilePath != "" {
		node.FilePaths = []string{inst.FilePath}
	}

	for _, child := range inst.Children {
		if c := convertToSourcemapNode(child); c != nil {
			node.Children = append(node.Children, c)
		}
	}

	return node
}
