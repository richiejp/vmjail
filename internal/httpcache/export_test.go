package httpcache

import "os"

// Test-only accessors for the fs seams. Kept in export_test.go so
// the seams stay unexported in production builds.

func SetRenameFn(f func(oldpath, newpath string) error) { renameFn = f }

func SetCreateTempFn(f func(dir, pattern string) (*os.File, error)) { createTempFn = f }

func ResetSeams() {
	renameFn = os.Rename
	createTempFn = os.CreateTemp
}
