package exec

import (
	"fmt"
	"strings"
)

func ExitCodeFromExecError(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	const prefix = "command terminated with exit code "
	if strings.HasPrefix(msg, prefix) {
		var code int
		if _, e := fmt.Sscanf(msg[len(prefix):], "%d", &code); e == nil {
			return code
		}
	}
	return 1
}
