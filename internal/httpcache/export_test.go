package httpcache

import (
	"os"
	"time"
)

// Test-only accessors for the fs + clock seams. Kept in
// export_test.go so the seams stay unexported in production builds.

func SetRenameFn(f func(oldpath, newpath string) error) { renameFn = f }

func SetCreateTempFn(f func(dir, pattern string) (*os.File, error)) { createTempFn = f }

func SetNowNanos(f func() int64) { nowNanos = f }

func ResetSeams() {
	renameFn = os.Rename
	createTempFn = os.CreateTemp
	nowNanos = func() int64 { return time.Now().UnixNano() }
}
