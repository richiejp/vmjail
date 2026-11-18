package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePortMappingArg splits "8080:80" into (local, container).
// Rejects zero ports, ports > 65535, missing colon, or extra
// colons — the CLI's first-line validation before opening listeners.
func ParsePortMappingArg(s string) (local, container uint32, err error) {
	localStr, containerStr, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("port mapping: expected <local>:<container>, got %q", s)
	}
	if strings.Contains(containerStr, ":") {
		return 0, 0, fmt.Errorf("port mapping: too many ':' in %q", s)
	}
	local, err = parsePortValue(localStr, "local")
	if err != nil {
		return 0, 0, err
	}
	container, err = parsePortValue(containerStr, "container")
	if err != nil {
		return 0, 0, err
	}
	return local, container, nil
}

// parsePortValue is the per-side port validator: non-empty,
// numeric, in [1, 65535].
func parsePortValue(s, side string) (uint32, error) {
	if s == "" {
		return 0, fmt.Errorf("port mapping: empty %s port", side)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("port mapping: bad %s port %q: %w", side, s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("port mapping: %s port must be non-zero", side)
	}
	if n > 65535 {
		return 0, fmt.Errorf("port mapping: %s port %d out of range", side, n)
	}
	return uint32(n), nil
}
