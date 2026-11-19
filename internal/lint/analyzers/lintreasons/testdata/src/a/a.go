// Package a is the fixture for lintreasons' analysistest run. Each
// var name encodes what the directive on its line is supposed to do.
package a

// hasReason carries a valid suppression; the analyzer must not fire.
var hasReason int //vmjail:lint:disable=foo reason="valid suppression"

// noReason omits the reason clause entirely; analyzer must fire.
var noReason int //vmjail:lint:disable=foo // want `missing or empty reason`

// emptyReason supplies an empty quoted reason; analyzer must fire.
var emptyReason int //vmjail:lint:disable=foo reason="" // want `missing or empty reason`

// whitespaceReason has reason content that is only whitespace;
// analyzer must fire because it is the same review-time failure as
// an empty reason.
var whitespaceReason int //vmjail:lint:disable=foo reason="   " // want `missing or empty reason`

// multiAnalyzer disables two analyzers but supplies one reason;
// that is sufficient — one reason covers the disable line.
var multiAnalyzer int //vmjail:lint:disable=foo,bar reason="covers both"

// notDirective is an unrelated comment; analyzer must not fire.
var notDirective int // a regular trailing comment

// spaceAfterSlashes also gets caught — the directive prefix is
// anchored at the start of the comment body modulo whitespace, so
// a stray space doesn't slip past the gate.
var spaceAfterSlashes int // vmjail:lint:disable=foo // want `missing or empty reason`

// embeddedQuoteInDoc references the directive in prose; analyzer
// must NOT fire — the prefix isn't anchored at the start.
var embeddedQuoteInDoc int // see vmjail:lint:disable=foo for the format

// bareDirective lists no analyzers; analyzer must fire with the
// "must list at least one analyzer name" diagnostic.
var bareDirective int //vmjail:lint:disable= // want `must list at least one`
