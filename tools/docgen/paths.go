package main

import (
	"path/filepath"
	"runtime"
)

// repoRoot is the holaclaw repository root, computed from this source file's
// own location so path lookups work the same way under `go run` (cwd = repo
// root) and `go test` (cwd = this package's directory).
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	// file = <repoRoot>/tools/docgen/paths.go
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}()

func inRepo(rel string) string {
	return filepath.Join(repoRoot, rel)
}
