//go:build !linux

package agent

func defaultSecretDeps() secretDeps { return secretDeps{} }
