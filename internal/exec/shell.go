package exec

import (
	"fmt"
	"strings"
)

func BuildRemoteShell(command string, args []string, env map[string]string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("empty command")
	}
	var b strings.Builder
	for k, v := range env {
		if strings.Contains(k, "=") || strings.TrimSpace(k) == "" {
			continue
		}
		b.WriteString("export ")
		b.WriteString(shellQuote(k))
		b.WriteString("=")
		b.WriteString(shellQuote(v))
		b.WriteString("; ")
	}
	b.WriteString(command)
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(shellQuote(a))
	}
	return b.String(), nil
}

func shellQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'"'"'`) + `'`
}
