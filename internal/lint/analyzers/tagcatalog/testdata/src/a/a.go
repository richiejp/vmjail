//go:build linux

// Package a is the fixture for tagcatalog. The build constraint
// above uses an allowlisted tag (linux); analyzer must not fire.
package a

func a() {}
