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

// Package test implements composite resource rendering and testing.
package test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/spf13/afero"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane/v2/cmd/crank/beta/validate"
	"github.com/crossplane/crossplane/v2/cmd/crank/common/load"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Cmd arguments and flags for alpha render test subcommand.
type Cmd struct {
	// Arguments.
	TestDir string `arg:"" default:"tests" help:"Directory containing test cases." type:"path"`

	// Flags. Keep them in alphabetical order.
	FunctionAnnotations  []string      `help:"Override function annotations for all functions. Can be repeated." short:"a"`
	FunctionsFile        string        `help:"Path to functions file for function resolution."`
	IncludeFullXR        bool          `default:"false" help:"Include a direct copy of the input XR's spec and metadata fields in the rendered output." short:"x"`
	OutputFile           string        `default:"expected.yaml"                                                  help:"Name of the output file (used when not comparing)."`
	PackageFile          string        `help:"Path to package file for resolving function versions."`
	Timeout              time.Duration `default:"1m"                                                             help:"How long to run before timing out."`
	WriteExpectedOutputs bool          `default:"false"                                                          help:"Write/update expected.yaml files instead of comparing." short:"w"`

	// validation flags
	CacheDir              string `default:"~/.crossplane/cache" help:"Path to the cache directory where downloaded schemas are stored. May include ~ for the home directory." predictor:"directory" group:"validation"`
	CleanCache            bool   `help:"Clean the cache directory before downloading package schemas." default:"false" group:"validation"`
	ErrorOnMissingSchemas bool   `default:"false" help:"Return non zero exit code if not all schemas are provided." group:"validation"`
	SkipSuccessResults    bool   `help:"Skip printing success results." group:"validation"`
	Validate              bool   `default:"false" help:"Validate XR and managed resources based on their XRD and OpenAPI schemas" group:"validation"`
	CrossplaneImage       string `help:"Specify the Crossplane image to be used for validating the built-in schemas." group:"validation"`

	fs afero.Fs
}

// Help prints out the help for the alpha render test command.
func (c *Cmd) Help() string {
	return `
Render composite resources (XRs) and assert results.

This command renders XRs and compares them with expected outputs by default.
Use --write-expected-outputs to generate/update expected.yaml files.

Function resolution (at least one is required):
  - Provide --package-file to resolve functions from a package file
  - Provide --functions-file to load functions from a specific file
  - If both are provided, the functions-file takes precedence over the package file for any overlapping functions
  - If neither is provided, the composition must not reference any functions

Function annotations:
  - Use --function-annotations to override annotations for all functions
  - Useful for setting network configuration, environment variables, etc.

Examples:

    # Compare actual outputs with expected.yaml files (default)
    crossplane alpha render test --functions-file=functions.yaml

	# Generate/update expected.yaml files
    crossplane alpha render test --functions-file=functions.yaml --write-expected-outputs

	# Use package.yaml to auto-resolve function versions
    crossplane alpha render test --package-file=apis/package.yaml

	# Use both: package.yaml for defaults, custom functions file for overrides
    crossplane alpha render test --package-file=apis/package.yaml --functions-file=functions.yaml

	# Use custom Docker network for all functions
    crossplane alpha render test --package-file=apis/package.yaml \
      -a render.crossplane.io/runtime-docker-network=devnet

    # Test a specific directory
    crossplane alpha render test tests/my-test --functions-file=functions.yaml

    # Generate outputs with a different filename
    crossplane alpha render test --functions-file=functions.yaml --write-expected-outputs --output-file=snapshot.yaml
`
}

// AfterApply implements kong.AfterApply.
func (c *Cmd) AfterApply() error {
	c.fs = afero.NewOsFs()
	return nil
}

// Run alpha render test.
func (c *Cmd) Run(k *kong.Context, log logging.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	if c.Validate {
		log.Info("Validating XR and managed resources")

		testDirs, err := findTestDirectories(c.fs, c.TestDir)
		if err != nil {
			return fmt.Errorf("failed to find test directories: %v", err)
		}

		if err := c.validate(k, testDirs); err != nil {
			return fmt.Errorf("validation failed: %v", err)
		}

		return nil
	}

	// Run the test
	result, err := Test(ctx, log, Inputs{
		TestDir:              c.TestDir,
		FileSystem:           c.fs,
		WriteExpectedOutputs: c.WriteExpectedOutputs,
		OutputFile:           c.OutputFile,
		PackageFile:          c.PackageFile,
		FunctionsFile:        c.FunctionsFile,
		FunctionAnnotations:  c.FunctionAnnotations,
		IncludeFullXR:        c.IncludeFullXR,
	})
	if err != nil {
		return err
	}

	if !result.Pass {
		return errors.New("test failed: differences found between expected and actual outputs")
	}

	if !c.WriteExpectedOutputs {
		_, _ = fmt.Fprintln(os.Stdout, "All tests passed")
	}

	return nil
}

type ResourceWithSource struct {
	unstructured.Unstructured
	Source string
}

func (c *Cmd) validate(k *kong.Context, testDirs []string) error {
	crossplaneImage := c.CrossplaneImage
	if crossplaneImage == "" {
		resolvedCrossplaneImage, err := resolvePackageVersion("xpkg.crossplane.io/crossplane/crossplane", ">v2.0.0")
		if err != nil {
			return fmt.Errorf("failed to resolve crossplane version: %v", err)
		}

		crossplaneImage = resolvedCrossplaneImage
	}

	if len(c.PackageFile) == 0 {
		return fmt.Errorf("--package-file is required when validate is set")
	}

	// Load all extensions
	ext := filepath.Dir(c.PackageFile)
	extensionLoader, err := load.NewLoader(ext)
	if err != nil {
		return errors.Wrapf(err, "cannot load extensions from %q", ext)
	}

	extensions, err := extensionLoader.Load()
	if err != nil {
		return errors.Wrapf(err, "cannot load extensions from %q", ext)
	}

	if strings.HasPrefix(c.CacheDir, "~/") {
		homeDir, _ := os.UserHomeDir()
		c.CacheDir = filepath.Join(homeDir, c.CacheDir[2:])
	}

	m := validate.NewManager(c.CacheDir, c.fs, k.Stdout, validate.WithCrossplaneImage(crossplaneImage))

	// Convert XRDs/CRDs to CRDs and add package dependencies
	if err := m.PrepExtensions(extensions); err != nil {
		return errors.Wrapf(err, "cannot prepare extensions")
	}

	// Download package base layers to cache and load them as CRDs
	if err := m.CacheAndLoad(c.CleanCache); err != nil {
		return errors.Wrapf(err, "cannot download and load cache")
	}

	var (
		crds             = m.CRDs()
		validationErrors []string
		wg               sync.WaitGroup
		mu               sync.Mutex
	)

	for _, testDir := range testDirs {
		wg.Go(func() {
			if err := c.validateResource(filepath.Join(testDir, c.OutputFile), crds, k.Stdout); err != nil {
				mu.Lock()
				validationErrors = append(validationErrors, fmt.Sprintf("%s: %v", testDir, err))
				mu.Unlock()
			}
		})

		wg.Wait()
	}

	if len(validationErrors) > 0 {
		return errors.New(strings.Join(validationErrors, "\n"))
	}

	return nil
}

func (c Cmd) validateResource(path string, crds []*apiextv1.CustomResourceDefinition, out io.Writer) error {
	loader, err := load.NewLoader(path)
	if err != nil {
		return fmt.Errorf("failed to setup new loader: %v", err)
	}

	resources, err := loader.Load()
	if err != nil {
		return fmt.Errorf("failed to load resources: %v", err)
	}

	if cwd, err := os.Getwd(); err == nil {
		if relativePath, err := filepath.Rel(cwd, path); err == nil {
			path = relativePath
		}
	}

	return validate.SchemaValidation(
		context.Background(),
		resources,
		crds,
		c.ErrorOnMissingSchemas,
		c.SkipSuccessResults,
		&prefixWriter{w: out, prefix: fmt.Sprintf("[%s] ", path)},
	)
}

type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	_, err := fmt.Fprintf(p.w, "%s%s", p.prefix, b)
	return len(b), err
}
