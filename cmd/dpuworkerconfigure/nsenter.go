package dpuworkerconfigure

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// nsenterRun executes a command on the host via nsenter into PID 1.
func nsenterRun(command string, args ...string) (string, error) {
	nsenterArgs := []string{"--target", "1", "--mount", "--uts", "--ipc", "--net", "--", command}
	nsenterArgs = append(nsenterArgs, args...)
	cmd := exec.Command("nsenter", nsenterArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nsenter %s %s failed: %w\nstderr: %s", command, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
