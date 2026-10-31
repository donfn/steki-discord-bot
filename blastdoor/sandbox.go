package blastdoor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// libheif and zstd are the only code here not written in a memory-safe
// language, so they are where an exploit would land. Railway cannot block a
// service's outbound IPv4, so each tool runs without the means to use a network
// at all: the binary starts itself with sandboxExecFlag, installs a seccomp
// filter (sandbox_linux.go) and then becomes the tool, which keeps the filter
// through any further exec.
const (
	sandboxExecFlag  = "--sandbox-exec"
	sandboxProbeFlag = "--sandbox-probe"
	toolWaitDelay    = 5 * time.Second
)

var selfPath, selfPathErr = os.Executable()

// sandboxCommand runs tool with only env and a memory ceiling, sandboxed where
// the platform can. Elsewhere, which means a developer's machine, it only gets
// the ceiling.
func sandboxCommand(ctx context.Context, memoryKiB int, env []string, tool string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if sandboxSupported {
		cmd = exec.CommandContext(ctx, selfPath, append([]string{sandboxExecFlag, strconv.Itoa(memoryKiB), tool}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", append([]string{"-c",
			fmt.Sprintf(`ulimit -v %d 2>/dev/null; exec "$0" "$@"`, memoryKiB), tool}, args...)...)
	}
	cmd.Env = env
	// Something the tool left behind must not hold its pipes open forever.
	cmd.WaitDelay = toolWaitDelay
	return cmd
}

// CheckSandbox proves the sandbox works on this host before the service takes
// any file: it fails if a sandboxed process can open a socket, or if a tool
// cannot run inside the sandbox. The service exits on an error, its healthcheck
// fails, and the previous deployment keeps serving.
func CheckSandbox(ctx context.Context) error {
	if !sandboxSupported {
		return nil
	}
	if selfPathErr != nil {
		return selfPathErr
	}
	if msg, err := exec.CommandContext(ctx, selfPath, sandboxProbeFlag).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox probe: %w: %s", err, bytes.TrimSpace(msg))
	}
	cmd := zstdCommand(ctx, compressMemoryKiB, "-1", "-q", "-c")
	cmd.Stdin = strings.NewReader("blastdoor")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("zstd in the sandbox: %w", err)
	}
	if len(out) == 0 {
		return errors.New("zstd in the sandbox wrote nothing")
	}
	return nil
}
