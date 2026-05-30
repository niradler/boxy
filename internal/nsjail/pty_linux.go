//go:build linux

package nsjail

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"boxy.dev/boxy/internal/api"
)

// Slave must be closed in the parent after cmd.Start() so master reaches EOF when the child exits.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			master.Close()
			master = nil
		}
	}()

	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(),
		syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", e)
	}

	var slaveNum uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(),
		syscall.TIOCGPTN, uintptr(unsafe.Pointer(&slaveNum))); e != 0 {
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", e)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", slaveNum)
	slave, err = os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open slave PTY %s: %w", slavePath, err)
	}
	return master, slave, nil
}

// ctx must be the deadline context that was used to create nsjailCmd.
func (a *NsjailAdapter) execPTY(
	ctx context.Context,
	nsjailCmd *exec.Cmd,
	timeoutSecs int,
) (*api.ExecResponseBody, error) {
	master, slave, err := openPTY()
	if err != nil {
		return nil, errInternal(fmt.Sprintf("allocate PTY: %v", err))
	}

	nsjailCmd.Stdin = slave
	nsjailCmd.Stdout = slave
	nsjailCmd.Stderr = slave
	nsjailCmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	if err := nsjailCmd.Start(); err != nil {
		slave.Close()
		master.Close()
		return nil, errInternal(fmt.Sprintf("nsjail start: %v", err))
	}

	slave.Close()

	var out limitWriter
	if a.cfg.MaxOutputBytes > 0 {
		out.limit = int64(a.cfg.MaxOutputBytes)
	}
	readDone := make(chan struct{})
	go func() {
		io.Copy(&out, master) //nolint:errcheck
		master.Close()
		close(readDone)
	}()

	waitErr := nsjailCmd.Wait()
	<-readDone

	return a.buildExecResponse(waitErr, ctx, timeoutSecs, out.String(), "")
}
