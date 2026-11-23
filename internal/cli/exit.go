package cli

import "fmt"

// ExitError preserves the workload's exit status independently of setup errors.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("workload exited with status %d", e.Code) }
