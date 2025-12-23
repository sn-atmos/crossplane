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
	"time"

	"github.com/alecthomas/kong"
	"github.com/spf13/afero"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// Cmd arguments and flags for alpha render test subcommand.
type Cmd struct {
	// Arguments.
	TestDir string `arg:"" default:"tests" help:"Directory containing test cases." type:"path"`

	// Flags. Keep them in alphabetical order.
	FunctionsFile        string        `help:"Path to functions file (default: dev-functions.yaml)."`
	OutputFile           string        `default:"expected.yaml" help:"Name of the output file (used when not comparing)."`
	PackageFile          string        `help:"Path to package.yaml file for resolving function versions."`
	Timeout              time.Duration `default:"1m"            help:"How long to run before timing out."`
	WriteExpectedOutputs bool          `default:"false"         help:"Write/update expected.yaml files instead of comparing."       short:"w"`

	fs afero.Fs
}

// Help prints out the help for the alpha render op command.
func (c *Cmd) Help() string {
	return `
Render composite resources (XRs) and assert results.

This command renders XRs and compares them with expected outputs by default.
Use --write-expected-outputs to generate/update expected.yaml files.

Function resolution:
  - If --package-file is provided, functions are resolved from package.yaml
  - If --functions-file is provided, functions are loaded from that file
  - If both are provided, functions-file takes precedence (allows overrides)
  - Default functions file is dev-functions.yaml (if it exists)

Examples:

    # Compare actual outputs with expected.yaml files (default)
    crossplane alpha render test

	# Generate/update expected.yaml files
    crossplane alpha render test --write-expected-outputs

	# Use package.yaml to auto-resolve function versions
    crossplane alpha render test --package-file=apis/package.yaml

	# Use a custom functions file
    crossplane alpha render test --functions-file=my-functions.yaml

	# Use both: package.yaml for defaults, custom functions file for overrides
    crossplane alpha render test --package-file=apis/package.yaml --functions-file=local-dev.yaml

    # Test a specific directory
    crossplane alpha render test tests/my-test

    # Generate outputs with a different filename
    crossplane alpha render test --write-expected-outputs --output-file=snapshot.yaml
`
}

// AfterApply implements kong.AfterApply.
func (c *Cmd) AfterApply() error {
	c.fs = afero.NewOsFs()
	return nil
}

// Run alpha render test.
func (c *Cmd) Run(_ *kong.Context, log logging.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	// Run the test
	result, err := Test(ctx, log, Inputs{
		TestDir:              c.TestDir,
		FileSystem:           c.fs,
		WriteExpectedOutputs: c.WriteExpectedOutputs,
		OutputFile:           c.OutputFile,
		PackageFile:          c.PackageFile,
		FunctionsFile:        c.FunctionsFile,
	})
	if err != nil {
		return err
	}

	if !result.Pass {
		return errors.New("test failed")
	}

	return nil
}
