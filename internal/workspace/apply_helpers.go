package workspace

import "time"

// osChtimesTime converts a Unix-nanos timestamp from a manifest
// entry into a time.Time suitable for os.Chtimes. Hoisted out of
// apply.go so the file's logic stays linear.
func osChtimesTime(unixNano int64) time.Time {
	return time.Unix(0, unixNano)
}
