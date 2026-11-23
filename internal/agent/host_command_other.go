//go:build !linux

package agent

import (
	"context"
	"errors"
	"net"
)

const DefaultCommandVsockPort uint32 = 9600

func runHostCommand(context.Context, string, func(context.Context, uint32) (net.Conn, error)) error {
	return errors.New("host command execution requires Linux")
}
