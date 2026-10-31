//go:build linux && (amd64 || arm64)

package blastdoor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sandboxSupported = true
	// sandboxFailedExit tells a sandbox that could not start from a tool's own
	// failure.
	sandboxFailedExit = 125
)

// The filter's return values, from linux/seccomp.h.
const (
	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	// x32Bit marks x32 syscalls on amd64, which would otherwise slip past the
	// numbers below under numbers of their own.
	x32Bit = 0x40000000
)

// deniedSyscalls is a denylist, because the tools' needs change with their
// versions and a wrongly refused call would break every file read. It covers
// what the sandbox is for: no socket can be made, directly or through io_uring,
// and other processes cannot be reached. The rest is kernel surface these tools
// have no use for.
var deniedSyscalls = []uintptr{
	unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_IO_URING_SETUP,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD, unix.SYS_UNSHARE, unix.SYS_SETNS,
}

// init turns this process into the sandbox when started for that. It runs
// before main so nothing of the service starts first, and in the test binary
// as well, which is how the tests reach it.
func init() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case sandboxExecFlag:
		sandboxFatal(sandboxExec(os.Args[2:]))
	case sandboxProbeFlag:
		sandboxFatal(sandboxProbe())
		os.Exit(0)
	}
}

func sandboxFatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "blastdoor sandbox:", err)
		os.Exit(sandboxFailedExit)
	}
}

// sandboxExec applies the sandbox and becomes the tool. It only returns on
// failure, so a tool never runs without the filter.
func sandboxExec(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: memoryKiB tool [args]")
	}
	memoryKiB, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return err
	}
	path, err := exec.LookPath(args[1])
	if err != nil {
		return err
	}
	if err := installFilter(); err != nil {
		return err
	}
	limit := &syscall.Rlimit{Cur: memoryKiB << 10, Max: memoryKiB << 10}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, limit); err != nil {
		return err
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{}); err != nil {
		return err
	}
	return syscall.Exec(path, args[1:], os.Environ())
}

// sandboxProbe checks that the filter really refuses sockets on this host.
func sandboxProbe() error {
	if err := installFilter(); err != nil {
		return err
	}
	for _, family := range []int{unix.AF_INET, unix.AF_INET6, unix.AF_UNIX} {
		fd, err := unix.Socket(family, unix.SOCK_STREAM, 0)
		if err == nil {
			unix.Close(fd)
			return fmt.Errorf("opened a socket of family %d", family)
		}
		if !errors.Is(err, unix.EPERM) {
			return fmt.Errorf("socket of family %d: %w, not refused by the filter", family, err)
		}
	}
	return nil
}

// installFilter applies the filter to the calling thread, the one that goes on
// to exec. Other threads of this short-lived process end at the exec.
func installFilter() error {
	runtime.LockOSThread()
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return errors.New("no filter for " + runtime.GOARCH)
	}
	filter := seccompProgram(arch)
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	return nil
}

// seccompProgram kills a process using any other architecture's syscall
// numbers, such as 32-bit calls on amd64, refuses deniedSyscalls and allows the
// rest. seccomp_data holds the syscall number at offset 0 and the architecture
// at offset 4.
func seccompProgram(arch uint32) []unix.SockFilter {
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
	}
	const (
		load = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		ret  = unix.BPF_RET | unix.BPF_K
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
	)
	prog := []unix.SockFilter{
		stmt(load, 4),
		jump(jeq, arch, 1, 0),
		stmt(ret, seccompRetKillProcess),
		stmt(load, 0),
		jump(jge, x32Bit, 0, 1),
		stmt(ret, seccompRetKillProcess),
	}
	for _, nr := range deniedSyscalls {
		prog = append(prog, jump(jeq, uint32(nr), 0, 1), stmt(ret, seccompRetErrno|uint32(unix.EPERM)))
	}
	return append(prog, stmt(ret, seccompRetAllow))
}

// HardenProcess keeps the tools away from the server itself. They run as the
// same user, so without this one could open /proc/<server>/mem and carry on
// with the server's network access.
func HardenProcess() error {
	return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
