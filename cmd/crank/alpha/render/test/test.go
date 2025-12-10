/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

// Inputs contains all inputs to the test process
type Inputs struct {
	TestDir        string
	FileSystem     afero.Fs
	OutputFileName string // Output filename, defaults to "expected.yaml"
	CompareOutputs bool   // If true, compare actual vs. expected outputs using dyff
}

// Outputs contains test results
type Outputs struct {
	TestDirs []string // Directories containing tests
}

// testResult holds the result of processing a single test directory
type testResult struct {
	dir          string
	actualOutput []byte
	err          error
}

// Test
func Test(ctx context.Context, log logging.Logger, in Inputs) (Outputs, error) {

	outputFileName := in.OutputFileName
	if outputFileName == "" {
		outputFileName = "expected.yaml"
	}

	// Check if dev-functions.yaml exists
	if exists, _ := afero.Exists(in.FileSystem, "dev-functions.yaml"); !exists {
		return Outputs{}, errors.New("dev-functions.yaml not found")
	}

	// Find all directories with a composite-resource.yaml file
	testDirs, err := findTestDirectories(in.FileSystem, in.TestDir)
	if err != nil {
		return Outputs{}, err
	}

	// Print to stdout for verification
	fmt.Printf("Found %d test directories:\n", len(testDirs))
	for _, dir := range testDirs {
		fmt.Printf("  - %s\n", dir)
	}

	log.Info("Found test directories", "count", len(testDirs))

	// Process tests in parallel
	var wg sync.WaitGroup
	resultsChan := make(chan testResult, len(testDirs))

	for _, dir := range testDirs {
		wg.Add(1)
		go func(testDir string) {
			defer wg.Done()

			output, err := processTestDirectory(ctx, log, in.FileSystem, testDir)
			resultsChan <- testResult{
				dir:          testDir,
				actualOutput: output,
				err:          err,
			}
		}(dir)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	close(resultsChan)

	// Collect results
	results := make(map[string]testResult)
	for result := range resultsChan {
		if result.err != nil {
			return Outputs{}, errors.Wrapf(result.err, "failed to process %q", result.dir)
		}
		results[result.dir] = result
	}

	// If CompareOutputs is true, compare expected vs. actual
	if in.CompareOutputs {
		log.Info("Comparing outputs with dyff")
		hasErrors := false

		for _, dir := range testDirs {
			result := results[dir]
			expectedPath := filepath.Join(dir, "expected.yaml")

			// Check if expected file exists
			if exists, _ := afero.Exists(in.FileSystem, expectedPath); !exists {
				fmt.Printf("Warning: expected.yaml not found in %s, skipping comparison\n", dir)
				continue
			}

			// Read expected output
			expectedOutput, err := afero.ReadFile(in.FileSystem, expectedPath)
			if err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot read expected output from %q", expectedPath)
			}

			// Use temporary files for dyff
			tmpExpected, err := os.CreateTemp("", "expected-*.yaml")
			if err != nil {
				return Outputs{}, errors.Wrap(err, "cannot create temp file for expected output")
			}
			defer os.Remove(tmpExpected.Name())
			defer tmpExpected.Close()

			tmpActual, err := os.CreateTemp("", "actual-*.yaml")
			if err != nil {
				return Outputs{}, errors.Wrap(err, "cannot create temp file for actual output")
			}
			defer os.Remove(tmpActual.Name())
			defer tmpActual.Close()

			// Write contents to temp files
			if _, err := tmpExpected.Write(expectedOutput); err != nil {
				return Outputs{}, errors.Wrap(err, "cannot write expected output to temp file")
			}
			if _, err := tmpActual.Write(result.actualOutput); err != nil {
				return Outputs{}, errors.Wrap(err, "cannot write actual output to temp file")
			}

			// Close files before running dyff
			tmpExpected.Close()
			tmpActual.Close()

			// Run dyff with direct terminal output for colors
			cmd := exec.CommandContext(ctx, "dyff", "between", "--set-exit-code", "--omit-header", tmpExpected.Name(), tmpActual.Name())
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Run(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					// dyff returns non-zero exit code if files differ
					if exitErr.ExitCode() != 0 {
						fmt.Printf("\n❌ Differences found in %s:\n", dir)
						hasErrors = true
					}
				} else {
					fmt.Fprintf(os.Stderr, "Warning: dyff failed for %s: %v\n", dir, err)
					hasErrors = true
				}
			} else {
				fmt.Printf("✓ No differences in %s\n", dir)
			}
		}

		if hasErrors {
			return Outputs{}, errors.New("test failed: differences found between expected and actual outputs")
		}

		log.Info("All tests passed")
	} else {
		// If not comparing, write the outputs to files
		for _, dir := range testDirs {
			result := results[dir]
			outputPath := filepath.Join(dir, outputFileName)
			if err := afero.WriteFile(in.FileSystem, outputPath, result.actualOutput, 0644); err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot write output to %q", outputPath)
			}
			fmt.Printf("Wrote output to: %s\n", outputPath)
		}
	}

	return Outputs{TestDirs: testDirs}, nil
}

// findTestDirectories finds all directories containing a composite-resource.yaml file
func findTestDirectories(filesystem afero.Fs, testDir string) ([]string, error) {
	var testDirs []string

	err := afero.Walk(filesystem, testDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && info.Name() == "composite-resource.yaml" {
			testDirs = append(testDirs, filepath.Dir(path))
		}

		return nil
	})

	return testDirs, err
}

// processTestDirectory handles the rendering for a single test directory
func processTestDirectory(ctx context.Context, log logging.Logger, filesystem afero.Fs, dir string) ([]byte, error) {
	fmt.Printf("Processing test directory: %s\n", dir)

	compositeResourceFilePath := filepath.Join(dir, "composite-resource.yaml")
	compositeResource, err := render.LoadCompositeResource(filesystem, compositeResourceFilePath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot load CompositeResource from %q", compositeResourceFilePath)
	}

	// Extract composition name
	compositionName, err := findCompositionName(compositeResource)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot extract composition name from %q", compositeResourceFilePath)
	}
	fmt.Printf("Composition name: %s\n", compositionName)

	// Find the composition file
	compositionFilePath, err := findCompositionFile(filesystem, ".", compositionName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot find composition file for %q", compositionName)
	}

	fmt.Printf("Composition file: %s\n", compositionFilePath)

	// Load the composition
	composition, err := render.LoadComposition(filesystem, compositionFilePath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot load Composition from %q", compositionFilePath)
	}

	// Load functions from dev-functions.yaml
	functions, err := render.LoadFunctions(filesystem, "dev-functions.yaml")
	if err != nil {
		return nil, errors.Wrap(err, "cannot load functions from dev-functions.yaml")
	}

	// Build render inputs
	renderInputs := render.Inputs{
		CompositeResource: compositeResource,
		Composition:       composition,
		Functions:         functions,
	}

	// Check for optional extra resources
	extraResourcesPath := filepath.Join(dir, "extra-resources.yaml")
	if exists, _ := afero.Exists(filesystem, extraResourcesPath); exists {
		extraResources, err := render.LoadRequiredResources(filesystem, extraResourcesPath)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot load extra resources from %q", extraResourcesPath)
		}
		renderInputs.ExtraResources = extraResources
		fmt.Printf("Found extra resources: %s\n", extraResourcesPath)
	}

	// Check for optional observed resources
	observedResourcesPath := filepath.Join(dir, "observed-resources.yaml")
	if exists, _ := afero.Exists(filesystem, observedResourcesPath); exists {
		observedResources, err := render.LoadObservedResources(filesystem, observedResourcesPath)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot load observed resources from %q", observedResourcesPath)
		}
		renderInputs.ObservedResources = observedResources
		fmt.Printf("Found observed resources: %s\n", observedResourcesPath)
	}

	// Check for optional context files
	contextsDir := filepath.Join(dir, "contexts")
	if exists, _ := afero.DirExists(filesystem, contextsDir); exists {
		contextFiles, err := afero.ReadDir(filesystem, contextsDir)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot read contexts directory %q", contextsDir)
		}

		contextMap := make(map[string][]byte)
		for _, fileInfo := range contextFiles {
			if fileInfo.IsDir() {
				continue
			}

			// Only process .json files
			if filepath.Ext(fileInfo.Name()) != ".json" {
				continue
			}

			contextFilePath := filepath.Join(contextsDir, fileInfo.Name())
			contextData, err := afero.ReadFile(filesystem, contextFilePath)
			if err != nil {
				return nil, errors.Wrapf(err, "cannot read context file %q", contextFilePath)
			}

			// Use filename without extension as context name
			contextName := fileInfo.Name()[:len(fileInfo.Name())-len(".json")]
			contextMap[contextName] = contextData
			fmt.Printf("Found context: %s from %s\n", contextName, contextFilePath)
		}

		if len(contextMap) > 0 {
			renderInputs.Context = contextMap
		}
	}

	// Run render
	outputs, err := render.Render(ctx, log, renderInputs)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot render for %q", dir)
	}

	// Convert outputs to YAML
	var yamlDocs [][]byte

	// Add the composite resource
	xrYAML, err := yaml.Marshal(outputs.CompositeResource.Object)
	if err != nil {
		return nil, errors.Wrap(err, "cannot marshal composite resource to YAML")
	}
	yamlDocs = append(yamlDocs, xrYAML)

	// Add all composed resources
	for _, composed := range outputs.ComposedResources {
		composedYAML, err := yaml.Marshal(composed.Object)
		if err != nil {
			return nil, errors.Wrap(err, "cannot marshal composed resource to YAML")
		}
		yamlDocs = append(yamlDocs, composedYAML)
	}

	// Join all YAML documents with "---" separator
	var outputBytes []byte
	for i, doc := range yamlDocs {
		if i > 0 {
			outputBytes = append(outputBytes, []byte("\n---\n")...)
		}
		outputBytes = append(outputBytes, doc...)
	}

	return outputBytes, nil
}

// findCompositionName extracts the composition name from .spec.crossplane.compositionRef.name
func findCompositionName(compositeResource *composite.Unstructured) (string, error) {
	spec, ok := compositeResource.Object["spec"].(map[string]interface{})
	if !ok {
		return "", errors.New("spec not found in composite resource")
	}

	crossplane, ok := spec["crossplane"].(map[string]interface{})
	if !ok {
		return "", errors.New("spec.crossplane not found in composite resource")
	}

	compositionRef, ok := crossplane["compositionRef"].(map[string]interface{})
	if !ok {
		return "", errors.New("spec.crossplane.compositionRef not found in composite resource")
	}

	compositionName, ok := compositionRef["name"].(string)
	if !ok {
		return "", errors.New("spec.crossplane.compositionRef.name not found or not a string")
	}

	return compositionName, nil
}

// findCompositionFile searches for a Composition YAML file with the given composition name
func findCompositionFile(filesystem afero.Fs, searchDir, compositionName string) (string, error) {
	var compositionFile string

	err := afero.Walk(filesystem, searchDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Skip dev-extensions.yaml
		if info.Name() == "dev-extensions.yaml" {
			return nil
		}

		// Only check .yaml or .yml files
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		// Read and parse the file
		data, err := afero.ReadFile(filesystem, path)
		if err != nil {
			return nil // Skip files we can't read
		}

		// Check if this is a Composition with matching name
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}

		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil // Skip invalid YAML
		}

		// Check both kind and name match
		if doc.Kind == "Composition" && doc.Metadata.Name == compositionName {
			compositionFile = path
			return filepath.SkipAll // Found it, stop walking
		}

		return nil
	})

	if err != nil && err != filepath.SkipAll {
		return "", err
	}

	if compositionFile == "" {
		return "", errors.Errorf("composition %q not found", compositionName)
	}

	return compositionFile, nil
}
