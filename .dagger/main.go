// Package main implements the midea-mcp-control CI pipeline as a Dagger module.
//
// The pipeline is hermetic: it builds, tests and vets the project inside a
// pinned Go toolchain container. It never touches the user's Midea devices and
// never needs credentials, because the test suite uses fakes at every seam that
// would otherwise talk to hardware or the cloud.
//
// The "constraints" function encodes a project rule that is easy to break by
// accident and expensive to notice: the product must stay pure Go. No cgo, no
// os/exec, no Python.
package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/ci/internal/dagger"
)

// goToolchain is the image every step runs in. It is pinned to a minor
// version so a new patch release does not silently change CI results.
const goToolchain = "golang:1.26-bookworm"

// Ci is the pipeline entry point.
type Ci struct{}

// sourceOrWorkspace returns the directory to build from. When the caller does
// not pass one, the pipeline uses the workspace Dagger was invoked in, so
// `dagger call ci` works with no arguments from a shell or from CI.
func sourceOrWorkspace(source *dagger.Directory) *dagger.Directory {
	if source != nil {
		return source
	}
	return dag.CurrentWorkspace().Directory(".")
}

// goContainer returns a toolchain container with the source and caches mounted.
func (m *Ci) goContainer(source *dagger.Directory, cgo string) *dagger.Container {
	return dag.Container().
		From(goToolchain).
		WithMountedDirectory("/src", sourceOrWorkspace(source)).
		WithWorkdir("/src").
		WithEnvVariable("CGO_ENABLED", cgo).
		WithEnvVariable("GOFLAGS", "-mod=readonly").
		WithMountedCache("/go/pkg", dag.CacheVolume("midea-go-mod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("midea-go-build"))
}

// Test runs the unit test suite with cgo disabled, which is how the project
// is actually shipped.
func (m *Ci) Test(source *dagger.Directory) *dagger.Container {
	return m.goContainer(source, "0").WithExec([]string{"go", "test", "./..."})
}

// Vet runs go vet with cgo disabled.
func (m *Ci) Vet(source *dagger.Directory) *dagger.Container {
	return m.goContainer(source, "0").WithExec([]string{"go", "vet", "./..."})
}

// Race runs the test suite under the race detector. The detector needs cgo, so
// this step is the one place the pipeline enables it; it is a tooling
// requirement, not a property of the shipped binaries.
func (m *Ci) Race(source *dagger.Directory) *dagger.Container {
	return m.goContainer(source, "1").WithExec([]string{"go", "test", "-race", "./..."})
}

// Fmt fails when any Go file is not gofmt-clean. gofmt -l exits zero even when
// it lists files, so the check is done on its output.
func (m *Ci) Fmt(ctx context.Context, source *dagger.Directory) (string, error) {
	unformatted, err := m.goContainer(source, "0").
		WithExec([]string{"gofmt", "-l", "cmd", "internal"}).
		Stdout(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(unformatted) != "" {
		return "", fmt.Errorf("these files are not gofmt-clean:\n%s", unformatted)
	}
	return "gofmt clean", nil
}

// Constraints enforces the pure-Go rule. It is the one check that protects a
// promise made to anyone deploying this: the binary has no cgo and never shells
// out, and the repository carries no Python.
func (m *Ci) Constraints(ctx context.Context, source *dagger.Directory) (string, error) {
	constraints := m.goContainer(source, "0").WithExec([]string{
		"sh", "-c",
		// os/exec would let the tool spawn helper processes; import "C" would
		// pull in cgo; a Python file would drag in an interpreter we do not
		// support at runtime.
		"! grep -rn --include='*.go' -e '\"os/exec\"' -e 'import \"C\"' cmd internal && " +
			"! find . -name '*.py' -not -path './.git/*' | grep . && " +
			"echo 'pure-Go constraints satisfied'",
	})
	return constraints.Stdout(ctx)
}

// Build cross-compiles the CLI for one target with cgo disabled and returns
// the resulting binary.
func (m *Ci) Build(source *dagger.Directory, goos string, goarch string) *dagger.File {
	return m.goContainer(source, "0").
		WithEnvVariable("GOOS", goos).
		WithEnvVariable("GOARCH", goarch).
		WithExec([]string{"go", "build", "-trimpath", "-ldflags", "-s -w", "-o", "/out/midea-mcp-control", "./cmd/midea-mcp-control"}).
		File("/out/midea-mcp-control")
}

// BuildAll cross-compiles every supported target and returns the binaries.
func (m *Ci) BuildAll(source *dagger.Directory) *dagger.Directory {
	targets := []struct{ os, arch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
	}
	artifacts := dag.Directory()
	for _, target := range targets {
		name := fmt.Sprintf("%s-%s", target.os, target.arch)
		artifacts = artifacts.WithFile(
			fmt.Sprintf("midea-mcp-control-%s%s", name, windowsSuffix(target.os)),
			m.Build(source, target.os, target.arch),
		)
	}
	return artifacts
}

func windowsSuffix(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// Ci runs every check. This is what GitHub Actions calls, and it deliberately
// takes no arguments so `dagger call ci` works unattended: the source defaults
// to the workspace the command was invoked in.
func (m *Ci) Ci(ctx context.Context) (string, error) {
	source := sourceOrWorkspace(nil)
	steps := []struct {
		name string
		run  func() error
	}{
		{"gofmt", func() error { _, err := m.Fmt(ctx, source); return err }},
		{"pure-Go constraints", func() error { _, err := m.Constraints(ctx, source); return err }},
		{"vet", func() error { _, err := m.Vet(source).Sync(ctx); return err }},
		{"test", func() error { _, err := m.Test(source).Sync(ctx); return err }},
		{"race", func() error { _, err := m.Race(source).Sync(ctx); return err }},
		{"build", func() error { _, err := m.BuildAll(source).Sync(ctx); return err }},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			return "", fmt.Errorf("%s failed: %w", step.name, err)
		}
	}
	return fmt.Sprintf("all %d checks passed", len(steps)), nil
}
