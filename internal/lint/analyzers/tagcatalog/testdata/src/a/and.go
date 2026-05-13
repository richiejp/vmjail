//go:build linux && !sneaky

package a // want `sneaky.*not in tagcatalog`

func andExpr() {}
