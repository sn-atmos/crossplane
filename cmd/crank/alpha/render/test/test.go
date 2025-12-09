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
	"path/filepath"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane/v2/cmd/crank/render"
	"github.com/spf13/afero"
)

// Inputs contains all inputs to the test process.
type Inputs struct {
	TestDir    string
	FileSystem afero.Fs
}

// Outputs contains test results.
type Outputs struct {
	TestDirs []string // Directories containing tests
}

// Test
func Test(ctx context.Context, log logging.Logger, in Inputs) (Outputs, error) {

	//outputFileName := "expected.yaml"

	// TODOs: find the file paths, read the files, send them to the render function

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
	for _, dir := range testDirs {
		log.Info("Test directory", "path", dir)
	}

	for _, dir := range testDirs {
		fmt.Printf("Processing test directory: %s\n", dir)
		compositionFilePath := "/home/mimmig/github/sn-atmos/crossplane/apis/policy-exemption/composition.yaml" // TODO
		comp, err := render.LoadComposition(in.FileSystem, compositionFilePath)
		if err != nil {
			return Outputs{}, errors.Wrapf(err, "cannot load Composition from %q", compositionFilePath)
		}
		fmt.Printf("Type of comp: %T\n", comp)
		fmt.Printf("Comp value (Go format): %+v\n\n", comp)
	}

	// render.Render()

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
