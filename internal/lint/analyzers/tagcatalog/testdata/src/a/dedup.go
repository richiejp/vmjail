//go:build linux || quark || quark

package a // want `quark.*not in tagcatalog`

func dedup() {}
