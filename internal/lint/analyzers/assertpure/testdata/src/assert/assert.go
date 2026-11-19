// Package assert is the test stub stand-in for
// github.com/richiejp/vmjail/internal/assert. The analyzer
// identifies the real package by its declared name (`assert`) so a
// local stub with the same name works for analysistest.
package assert

// True mirrors the real signature; the body is irrelevant — the
// analyzer only inspects call sites.
func True(cond bool, format string, args ...any) {
	_ = cond
	_ = format
	_ = args
}

// NoErr mirrors the real signature.
func NoErr(err error, format string, args ...any) {
	_ = err
	_ = format
	_ = args
}
