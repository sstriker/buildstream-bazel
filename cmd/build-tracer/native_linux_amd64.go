//go:build linux && amd64

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// nativeBackendAvailable reports whether the build target
// supports the native ptrace backend. Compiled-in via the
// linux+amd64 build constraint on this file; the fallback
// (`native_other.go`) returns false on every other GOOS/GOARCH.
func nativeBackendAvailable() bool { return true }

// runNative spawns the build command under PTRACE_TRACEME,
// follows every fork/vfork/clone, and captures every successful
// execve into an strace-compatible text file.
//
// Output format mirrors `strace -f -e trace=execve` so
// convert-element-trace' existing parser handles both
// backends without changes:
//
//	1234  execve("/usr/bin/cc", ["cc", "-O2", "-o", "x", "x.c"], 0x0) = 0
//
// Returns the wrapped command's exit status (or 1 on tracer
// errors). Linux/amd64 only: register layout, syscall number,
// and argument calling convention are arch-specific; non-amd64
// targets fall back to the strace shim.
// pidState carries the per-tracee bookkeeping the loop needs
// across stops: whether the next syscall stop is enter or
// exit, plus argv/path/cwd captured at enter (so we can emit
// them on exit when we know the return value).
//
// openat capture: when captureReads is set on the tracer,
// openat-enter saves the path + flags so openat-exit can
// emit (or skip) based on the return value. Only successful
// reads (rax >= 0) emit; failed-open events are noise. Path
// filtering against the source root happens at canonicalize
// time, not here — keeps the tracer dumb.
type pidState struct {
	atEnter      bool
	execPath     string
	execArgv     []string
	execCwd      string
	openPath     string
	openFlags    uint64
	openCaptured bool
}

func runNative(out string, args []string, captureReads bool) int {
	// ptrace requires every ptrace call for a given tracee to
	// come from the same OS thread. Lock for the entire trace
	// so the Go runtime doesn't reschedule us mid-loop.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	traceFile, err := os.Create(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: open trace: %v\n", err)
		return 1
	}
	defer traceFile.Close()

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: start child: %v\n", err)
		return 1
	}
	rootPid := cmd.Process.Pid

	// Cmd.Start() with Ptrace=true does fork+PTRACE_TRACEME+exec;
	// the child stops at SIGTRAP after exec. Wait for that stop.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(rootPid, &ws, 0, nil); err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: initial wait: %v\n", err)
		return 1
	}
	if !ws.Stopped() {
		fmt.Fprintf(os.Stderr, "build-tracer: child not stopped after exec: %v\n", ws)
		return 1
	}

	// Synthesize the initial execve line — Go's
	// SysProcAttr.Ptrace does TRACEME-then-exec without the
	// SIGSTOP dance strace uses, so the root tracee's first
	// exec stops AFTER the syscall completed (before we
	// could install options). We know what command we asked
	// for, so emit it from cmd.Path / cmd.Args. cwd at this
	// point is the tracee's, which equals build-tracer's own
	// (we didn't fork into a subdir).
	rootCwd, _ := os.Getwd()
	emitExecve(traceFile, rootPid, rootCwd, cmd.Path, cmd.Args)

	// Set options on the root tracee. New children inherit them
	// automatically via PTRACE_O_TRACE{FORK,VFORK,CLONE}, so we
	// only call SetOptions once.
	opts := syscall.PTRACE_O_TRACESYSGOOD |
		syscall.PTRACE_O_TRACEFORK |
		syscall.PTRACE_O_TRACEVFORK |
		syscall.PTRACE_O_TRACECLONE |
		syscall.PTRACE_O_TRACEEXEC
	if err := syscall.PtraceSetOptions(rootPid, opts); err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: PTRACE_SETOPTIONS: %v\n", err)
		return 1
	}
	if err := syscall.PtraceSyscall(rootPid, 0); err != nil {
		fmt.Fprintf(os.Stderr, "build-tracer: PTRACE_SYSCALL: %v\n", err)
		return 1
	}

	// Per-PID state: tracking whether the next syscall stop
	// is enter or exit; argv/path captured at enter so we can
	// emit them on exit (when we know the return value). New
	// children added on PTRACE_EVENT_FORK / VFORK / CLONE.
	states := map[int]*pidState{
		rootPid: {atEnter: true},
	}

	rootExit := 0
	for len(states) > 0 {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if errors.Is(err, syscall.ECHILD) {
				break
			}
			fmt.Fprintf(os.Stderr, "build-tracer: Wait4: %v\n", err)
			return 1
		}

		st := states[pid]
		if st == nil {
			// New tracee that surfaced before the
			// PTRACE_EVENT_FORK/VFORK/CLONE notification on its
			// parent (rare race). Register and treat as if
			// next stop is syscall enter.
			st = &pidState{atEnter: true}
			states[pid] = st
		}

		switch {
		case ws.Exited():
			if pid == rootPid {
				rootExit = ws.ExitStatus()
			}
			delete(states, pid)
			continue
		case ws.Signaled():
			if pid == rootPid {
				rootExit = 128 + int(ws.Signal())
			}
			delete(states, pid)
			continue
		}

		if !ws.Stopped() {
			// Continued without stop (rare). Just keep going.
			_ = syscall.PtraceSyscall(pid, 0)
			continue
		}

		sigToInject := 0
		stopSig := ws.StopSignal()

		// Decode the stop reason. Three cases of interest:
		//   1. PTRACE_O_TRACE{FORK,VFORK,CLONE,EXEC} events:
		//      stopSig == SIGTRAP, status's high bits encode
		//      PTRACE_EVENT_*. New child: register state.
		//   2. Syscall stop: stopSig == SIGTRAP|0x80
		//      (because of PTRACE_O_TRACESYSGOOD).
		//   3. Other signal: pass through.
		switch {
		case stopSig == syscall.SIGTRAP|0x80:
			handleSyscallStop(pid, st, traceFile, captureReads)
		case stopSig == syscall.SIGTRAP:
			event := ws.TrapCause()
			switch event {
			case syscall.PTRACE_EVENT_FORK,
				syscall.PTRACE_EVENT_VFORK,
				syscall.PTRACE_EVENT_CLONE:
				newPid, err := syscall.PtraceGetEventMsg(pid)
				if err == nil {
					if _, ok := states[int(newPid)]; !ok {
						states[int(newPid)] = &pidState{atEnter: true}
					}
				}
			case syscall.PTRACE_EVENT_EXEC:
				// Successful exec — the syscall-exit stop
				// follows; argv we captured at enter is
				// emitted there. State stays valid.
			}
		default:
			// Real signal — pass it on.
			sigToInject = int(stopSig)
		}

		if err := syscall.PtraceSyscall(pid, sigToInject); err != nil {
			// Tracee likely exited mid-loop; let the next
			// Wait4 reap it.
		}
	}

	return rootExit
}

// handleSyscallStop processes one syscall-enter or syscall-exit
// stop. On enter for execve (rax syscall number 59 on amd64),
// captures argv from the tracee's memory; on the matching exit
// with rax == 0 (success), emits the trace line.
//
// When captureReads is true, also handles openat (syscall 257):
// captures the path + flags arg at enter; on exit, if the
// return value is a non-negative fd (= the open succeeded) AND
// the access mode is O_RDONLY (read-only — we only care about
// reads for the configure-time oracle), emits an openat trace
// line. Filtering to source-tree paths happens downstream at
// canonicalize time.
func handleSyscallStop(pid int, st *pidState, w io.Writer, captureReads bool) {
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
		// Tracee transient state; toggle and move on.
		st.atEnter = !st.atEnter
		return
	}
	const (
		sysExecve = 59  // amd64 syscall number for execve(2)
		sysOpenat = 257 // amd64 syscall number for openat(2)
	)
	if st.atEnter {
		switch regs.Orig_rax {
		case sysExecve:
			st.execPath = readCString(pid, uintptr(regs.Rdi), 4096)
			st.execArgv = readArgv(pid, uintptr(regs.Rsi))
			// Snapshot cwd at the moment of the syscall.
			// The new process inherits the cwd the parent
			// had when it called execve; reading after the
			// exec would show the new image's cwd which —
			// usually identical, but recursive make can
			// chdir mid-build, and we want the parent's.
			st.execCwd = readCwd(pid)
		case sysOpenat:
			if captureReads {
				// openat(int dirfd, const char *pathname,
				// int flags, mode_t mode) — pathname in
				// rsi, flags in rdx (we ignore dirfd; the
				// canonical filter only keeps absolute
				// paths anyway).
				st.openPath = readCString(pid, uintptr(regs.Rsi), 4096)
				st.openFlags = regs.Rdx
				st.openCaptured = true
			}
		}
		st.atEnter = false
		return
	}
	// syscall-exit: emit on success, drop on failure.
	if st.execPath != "" {
		// rax holds the return value on amd64. 0 == success;
		// anything else == errno (negative).
		if int64(regs.Rax) == 0 {
			emitExecve(w, pid, st.execCwd, st.execPath, st.execArgv)
		}
		st.execPath = ""
		st.execArgv = nil
		st.execCwd = ""
	}
	if st.openCaptured {
		// Successful open returns a non-negative fd; failed
		// open returns -errno (large unsigned when read
		// from rax). int64 cast surfaces the sign.
		ret := int64(regs.Rax)
		// O_RDONLY = 0; access mode lives in the low 2 bits
		// (O_ACCMODE = 3). We want flags & 3 == 0 (read
		// only). Skip O_WRONLY (1) and O_RDWR (2); writes
		// don't belong in the read oracle.
		const oAccMode = 3
		if ret >= 0 && (st.openFlags&oAccMode) == 0 {
			emitOpenat(w, pid, st.openPath, st.openFlags, ret)
		}
		st.openPath = ""
		st.openFlags = 0
		st.openCaptured = false
	}
	st.atEnter = true
}

// readCwd resolves /proc/<pid>/cwd to the tracee's current
// working directory. Best-effort: returns "" if the symlink
// can't be read (tracee disappeared, /proc not mounted, etc.).
// Used to disambiguate same-basename .o paths from recursive
// make invocations across SUBDIRS — without cwd, lib/foo.o
// and src/foo.o look identical to the converter.
func readCwd(pid int) string {
	target, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return target
}

// readCString reads a NUL-terminated string from the tracee's
// memory at addr, capped at maxBytes for safety. Uses
// PTRACE_PEEKDATA which reads 8 bytes per call — fine for the
// short argv strings we encounter (paths, flags, source
// names).
func readCString(pid int, addr uintptr, maxBytes int) string {
	if addr == 0 {
		return ""
	}
	var b strings.Builder
	var word [8]byte
	for i := 0; i < maxBytes; i += 8 {
		if _, err := syscall.PtracePeekData(pid, addr+uintptr(i), word[:]); err != nil {
			break
		}
		for j := 0; j < 8; j++ {
			if word[j] == 0 {
				return b.String()
			}
			b.WriteByte(word[j])
		}
	}
	return b.String()
}

// readArgv reads a NULL-terminated array of char* from the
// tracee at addr, returning each pointer's referenced
// C-string. Caps at 4096 entries (vastly more than any real
// argv) to stay bounded on malformed memory.
func readArgv(pid int, addr uintptr) []string {
	if addr == 0 {
		return nil
	}
	var out []string
	var word [8]byte
	for i := 0; i < 4096; i++ {
		if _, err := syscall.PtracePeekData(pid, addr+uintptr(i*8), word[:]); err != nil {
			break
		}
		ptr := uintptr(binary.LittleEndian.Uint64(word[:]))
		if ptr == 0 {
			break
		}
		out = append(out, readCString(pid, ptr, 4096))
	}
	return out
}

// emitExecve writes one strace-compatible execve trace line to
// w. Format:
//
//	1234  cwd:"/build/root/lib"  execve("/usr/bin/cc", [...], 0x0) = 0
//
// The `cwd:"..."  ` prefix between the pid and the `execve(`
// call is a build-tracer extension over strace's plain output
// — captures the tracee's working directory at the moment of
// the syscall, so recursive-make builds (where each subdir
// invocation produces same-basename .o files in different
// dirs) disambiguate at correlation time. cwd is omitted when
// /proc/<pid>/cwd couldn't be read; convert-element-trace'
// parser tolerates both forms.
//
// Strings are quoted via straceQuote to escape control chars
// and embedded quotes the same way strace does. The trailing
// `0x0` is a placeholder for the envp argument (which strace
// renders as a real address); convert-element-trace'
// parser doesn't care about it.
func emitExecve(w io.Writer, pid int, cwd, path string, argv []string) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d  ", pid)
	if cwd != "" {
		b.WriteString("cwd:")
		straceQuote(&b, cwd)
		b.WriteString("  ")
	}
	b.WriteString("execve(")
	straceQuote(&b, path)
	b.WriteString(", [")
	for i, a := range argv {
		if i > 0 {
			b.WriteString(", ")
		}
		straceQuote(&b, a)
	}
	b.WriteString("], 0x0) = 0\n")
	_, _ = w.Write(b.Bytes())
}

// emitOpenat writes one strace-compatible openat trace line to
// w. Format:
//
//	1234  openat(AT_FDCWD, "/path", O_RDONLY|O_CLOEXEC) = 3
//
// Flags are rendered as a small subset of strace's flag-name
// decoding (the only ones glibc routinely passes for read-mode
// opens). canonicalize doesn't depend on the exact flag names —
// it filters by path against --source-root and strips the
// `= <fd>` suffix unconditionally — so the precise spelling
// here is for human readability, not byte stability.
func emitOpenat(w io.Writer, pid int, path string, flags uint64, retFd int64) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%d  openat(AT_FDCWD, ", pid)
	straceQuote(&b, path)
	b.WriteString(", ")
	b.WriteString(formatOpenFlags(flags))
	fmt.Fprintf(&b, ") = %d\n", retFd)
	_, _ = w.Write(b.Bytes())
}

// formatOpenFlags renders the open(2) flags arg in strace's
// `O_FOO|O_BAR` style. We model only the flags successful
// read-mode opens carry in practice (O_RDONLY, O_CLOEXEC,
// O_NOFOLLOW, O_DIRECTORY); other bits collapse into a hex
// suffix. Stable enough for the canonical-trace shape; we
// never parse this back.
func formatOpenFlags(flags uint64) string {
	parts := []string{"O_RDONLY"}
	const (
		oCloexec   = 0x80000 // O_CLOEXEC
		oNofollow  = 0x20000 // O_NOFOLLOW
		oDirectory = 0x10000 // O_DIRECTORY
		oNonblock  = 0x800   // O_NONBLOCK
	)
	if flags&oCloexec != 0 {
		parts = append(parts, "O_CLOEXEC")
		flags &^= oCloexec
	}
	if flags&oNofollow != 0 {
		parts = append(parts, "O_NOFOLLOW")
		flags &^= oNofollow
	}
	if flags&oDirectory != 0 {
		parts = append(parts, "O_DIRECTORY")
		flags &^= oDirectory
	}
	if flags&oNonblock != 0 {
		parts = append(parts, "O_NONBLOCK")
		flags &^= oNonblock
	}
	flags &^= 3 // O_RDONLY/WRONLY/RDWR already rendered as O_RDONLY
	out := strings.Join(parts, "|")
	if flags != 0 {
		out += fmt.Sprintf("|0x%x", flags)
	}
	return out
}

// straceQuote writes s into w as a strace-compatible quoted
// string: surrounded by double-quotes; \, ", and the
// non-printable ASCII range escaped as \\, \", \xNN.
func straceQuote(w *bytes.Buffer, s string) {
	w.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			w.WriteString(`\\`)
		case '"':
			w.WriteString(`\"`)
		case '\n':
			w.WriteString(`\n`)
		case '\t':
			w.WriteString(`\t`)
		case '\r':
			w.WriteString(`\r`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(w, `\x%02x`, c)
			} else {
				w.WriteByte(c)
			}
		}
	}
	w.WriteByte('"')
}
