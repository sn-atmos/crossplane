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
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/gonvenience/bunt"
	"github.com/gonvenience/ytbx"
	"github.com/homeport/dyff/pkg/dyff"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	v1 "github.com/crossplane/crossplane/v2/apis/apiextensions/v1"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
)

const (
	// CompositeFileName is the name of the file containing the composite resource.
	CompositeFileName = "composite-resource.yaml"
)

// Inputs contains all inputs to the test process.
type Inputs struct {
	TestDir          string
	FileSystem       afero.Fs
	OutputFile       string // Output filename, defaults to "expected.yaml"
	CompareOutputs   bool   // If true, compare actual vs. expected outputs using dyff
}

// Outputs contains test results.
type Outputs struct {
	TestDirs []string // Directories containing tests
}

// Test.
func Test(ctx context.Context, log logging.Logger, in Inputs) (Outputs, error) {
	outputFile := in.OutputFile

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

	// Process tests sequentially
	results := make(map[string][]byte)
	for _, dir := range testDirs {
		output, err := processTestDirectory(ctx, log, in.FileSystem, dir)
		if err != nil {
			return Outputs{}, errors.Wrapf(err, "failed to process %q", dir)
		}
		results[dir] = output
	}

	// If CompareOutputs is true, compare expected vs. actual
	if in.CompareOutputs {
		log.Info("Comparing outputs with dyff")
		hasErrors := false

		for _, dir := range testDirs {
			actualOutput := results[dir]
			expectedPath := filepath.Join(dir, "expected.yaml")

			// Read expected output
			expectedOutput, err := afero.ReadFile(in.FileSystem, expectedPath)
			if err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot read expected output from %q", expectedPath)
			}

			// Parse YAML documents using ytbx
			expectedDocs, err := ytbx.LoadDocuments(expectedOutput)
			if err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot parse expected YAML for %q", dir)
			}

			actualDocs, err := ytbx.LoadDocuments(actualOutput)
			if err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot parse actual YAML for %q", dir)
			}

			// Compare using dyff library
			report, err := dyff.CompareInputFiles(
				ytbx.InputFile{Documents: expectedDocs},
				ytbx.InputFile{Documents: actualDocs},
			)
			if err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot compare files for %q", dir)
			}

			// Check if there are differences
			if len(report.Diffs) > 0 {
				fmt.Printf("\n❌ Differences found in %s:\n", dir)

				// Create a human-readable report
				reportWriter := &dyff.HumanReport{
					Report:     report,
					OmitHeader: true,
				}

				// Write report to stdout with colors
				var buf bytes.Buffer
				if err := reportWriter.WriteReport(&buf); err != nil {
					return Outputs{}, errors.Wrapf(err, "cannot write diff report for %q", dir)
				}

				// Print with colors if terminal supports it
				fmt.Print(bunt.Sprint(buf.String()))
				hasErrors = true
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
			actualOutput := results[dir]
			outputPath := filepath.Join(dir, outputFile)
			if err := afero.WriteFile(in.FileSystem, outputPath, actualOutput, 0o644); err != nil {
				return Outputs{}, errors.Wrapf(err, "cannot write output to %q", outputPath)
			}
			fmt.Printf("Wrote output to: %s\n", outputPath)
		}
	}

	return Outputs{TestDirs: testDirs}, nil
}

// findTestDirectories finds all directories containing a composite-resource.yaml file.
func findTestDirectories(filesystem afero.Fs, testDir string) ([]string, error) {
	var testDirs []string

	err := afero.Walk(filesystem, testDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && info.Name() == CompositeFileName {
			testDirs = append(testDirs, filepath.Dir(path))
		}

		return nil
	})

	return testDirs, err
}

// processTestDirectory handles the rendering for a single test directory.
func processTestDirectory(ctx context.Context, log logging.Logger, filesystem afero.Fs, dir string) ([]byte, error) {
	fmt.Printf("Processing test directory: %s\n", dir)

	compositeResourceFilePath := filepath.Join(dir, CompositeFileName)
	compositeResource, err := render.LoadCompositeResource(filesystem, compositeResourceFilePath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot load CompositeResource from %q", compositeResourceFilePath)
	}

	// Extract composition name
	compositionName, found, err := unstructured.NestedString(compositeResource.Object, "spec", "crossplane", "compositionRef", "name")
	if err != nil {
		return nil, errors.Wrapf(err, "cannot extract composition name from %q", compositeResourceFilePath)
	}
	if !found {
		return nil, errors.Errorf("spec.crossplane.compositionRef.name not found in %q", compositeResourceFilePath)
	}
	fmt.Printf("Composition name: %s\n", compositionName)

	// Find and load the composition
	composition, compositionFilePath, err := findComposition(filesystem, ".", compositionName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot find composition for %q", compositionName)
	}

	fmt.Printf("Composition file: %s\n", compositionFilePath)

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
	exists, err := afero.Exists(filesystem, extraResourcesPath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot check if extra resources file exists at %q", extraResourcesPath)
	}
	if exists {
		extraResources, err := render.LoadRequiredResources(filesystem, extraResourcesPath)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot load extra resources from %q", extraResourcesPath)
		}
		renderInputs.ExtraResources = extraResources
		fmt.Printf("Found extra resources: %s\n", extraResourcesPath)
	}

	// Check for optional observed resources
	observedResourcesPath := filepath.Join(dir, "observed-resources.yaml")
	exists, err = afero.Exists(filesystem, observedResourcesPath)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot check if observed resources file exists at %q", observedResourcesPath)
	}
	if exists {
		observedResources, err := render.LoadObservedResources(filesystem, observedResourcesPath)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot load observed resources from %q", observedResourcesPath)
		}
		renderInputs.ObservedResources = observedResources
		fmt.Printf("Found observed resources: %s\n", observedResourcesPath)
	}

	// Check for optional context files
	contextsDir := filepath.Join(dir, "contexts")
	exists, err = afero.DirExists(filesystem, contextsDir)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot check if contexts directory exists at %q", contextsDir)
	}
	if exists {
		contextFiles, err := afero.ReadDir(filesystem, contextsDir)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot read contexts directory %q", contextsDir)
		}

		contexts := make(map[string][]byte)
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
			contextName := strings.TrimSuffix(fileInfo.Name(), ".json")
			contexts[contextName] = contextData
			fmt.Printf("Found context: %s from %s\n", contextName, contextFilePath)
		}

		if len(contexts) > 0 {
			renderInputs.Context = contexts
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

	// Join with --- separator
	outputBytes := bytes.Join(yamlDocs, []byte("\n---\n"))

	return outputBytes, nil
}

// findComposition searches for a Composition YAML file with the given composition name.
func findComposition(filesystem afero.Fs, searchDir, compositionName string) (*v1.Composition, string, error) {
	var foundComposition *v1.Composition
	var compositionFile string

	err := afero.Walk(filesystem, searchDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Only check .yaml or .yml files
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		// Try to load as a Composition
		composition, err := render.LoadComposition(filesystem, path)
		if err != nil {
			// Only skip if it's not a composition; other errors should be returned
			if strings.Contains(err.Error(), "not a composition") {
				return nil // Not a Composition, skip
			}
			return err
		}

		// Check if this is the composition we're looking for
		if composition.Name == compositionName {
			foundComposition = composition
			compositionFile = path
			return filepath.SkipAll // Found it, stop walking
		}

		return nil
	})

	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, "", err
	}

	if foundComposition == nil {
		return nil, "", errors.Errorf("composition %q not found", compositionName)
	}

	return foundComposition, compositionFile, nil
}
