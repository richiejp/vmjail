// Package a is the fixture for nolinkname's analysistest.
package a

//go:linkname forbidden other.forbidden // want `forbidden by Power of 10`
var forbidden = 1

//daft:lint:disable=nolinkname reason="fixture exercises suppression"
//go:linkname suppressed other.suppressed
var suppressed = 2

// regularComment is an unrelated comment; analyzer must not fire.
var regularComment = 3 // some doc text

// nearMissPrefix references the form in prose; analyzer must not
// fire because the prefix is not anchored at comment start.
var nearMissPrefix = 4 // see go:linkname for context
