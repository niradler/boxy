//go:build !linux

package nsjail

import (
	"context"
	"os/exec"

	"boxy.dev/boxy/internal/api"
)

func (a *NsjailAdapter) execPTY(
	_ context.Context,
	_ *exec.Cmd,
	_ int,
) (*api.ExecResponseBody, error) {
	return nil, errInternal("PTY not supported on this platform")
}
