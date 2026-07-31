package w3c

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	// DriverReadyTimeout bounds how long we wait for a freshly spawned driver
	// to answer /status.
	DriverReadyTimeout = 15 * time.Second

	driverPollInitial = 50 * time.Millisecond
	driverPollMax     = 500 * time.Millisecond

	// Teardown escalation. Killing the driver outright orphans the browser it
	// spawned, which then keeps its user-data-dir alive after we have already
	// deleted our scratch dirs — so it gets a grace period first.
	driverExitGrace = 3 * time.Second
	driverTermGrace = 2 * time.Second

	logTailMaxBytes = 64 * 1024
	logTailMaxLines = 12
)

// DriverSpec describes a driver process to launch.
type DriverSpec struct {
	// Name labels the driver in error messages ("chromedriver", "geckodriver").
	Name string
	Path string
	Args []string
	Env  []string

	// LogPath, when set, is where the driver log ends up. chromedriver is told
	// to write it itself via --log-path; geckodriver only writes to stdout/err,
	// so it needs RedirectOutput.
	LogPath        string
	RedirectOutput bool
}

// DriverProcess owns a running driver child process and its log file.
type DriverProcess struct {
	name    string
	logPath string
	cmd     *exec.Cmd
	logFile *os.File
}

// StartDriver launches the driver and blocks until it answers /status on sess,
// or ctx expires. On any failure the process is torn down before returning, so
// a failed start never leaks a child.
func StartDriver(ctx context.Context, spec DriverSpec, sess *Session) (*DriverProcess, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = spec.Env

	d := &DriverProcess{name: spec.Name, logPath: spec.LogPath, cmd: cmd}

	if spec.LogPath != "" && spec.RedirectOutput {
		logFile, err := os.Create(spec.LogPath)
		if err != nil {
			return nil, fmt.Errorf("create %s log: %w", spec.Name, err)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		d.logFile = logFile
	}

	if err := cmd.Start(); err != nil {
		d.closeLog()
		return nil, fmt.Errorf("start %s: %w", spec.Name, err)
	}

	if err := d.waitReady(ctx, sess); err != nil {
		d.Stop()
		return nil, err
	}
	return d, nil
}

func (d *DriverProcess) waitReady(ctx context.Context, sess *Session) error {
	readyCtx, cancel := context.WithTimeout(ctx, DriverReadyTimeout)
	defer cancel()

	waitFor := driverPollInitial
	for {
		probeCtx, probeCancel := context.WithTimeout(readyCtx, time.Second)
		err := sess.Status(probeCtx)
		probeCancel()
		if err == nil {
			return nil
		}

		select {
		case <-readyCtx.Done():
			return fmt.Errorf("%s did not become ready: %w", d.name, readyCtx.Err())
		case <-time.After(waitFor):
		}

		// Clamped: geckodriver's copy of this loop was missing the clamp and
		// ran 50→100→200→400→800ms.
		waitFor = min(waitFor*2, driverPollMax)
	}
}

// Stop terminates the driver, escalating Wait → SIGTERM → SIGKILL, and releases
// the log file.
func (d *DriverProcess) Stop() {
	if d == nil {
		return
	}
	defer d.closeLog()

	if d.cmd == nil || d.cmd.Process == nil {
		return
	}

	done := make(chan error, 1)
	go func() { done <- d.cmd.Wait() }()

	select {
	case <-done:
		d.cmd = nil
		return
	case <-time.After(driverExitGrace):
	}

	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		d.cmd = nil
		return
	case <-time.After(driverTermGrace):
	}

	_ = d.cmd.Process.Kill()
	<-done
	d.cmd = nil
}

func (d *DriverProcess) closeLog() {
	if d.logFile != nil {
		_ = d.logFile.Close()
		d.logFile = nil
	}
}

// LogTail returns the last few lines of the driver log for error messages.
// Empty when debug logging is off.
func (d *DriverProcess) LogTail() string {
	if d == nil || strings.TrimSpace(d.logPath) == "" {
		return ""
	}
	file, err := os.Open(d.logPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}

	start := int64(0)
	if info.Size() > logTailMaxBytes {
		start = info.Size() - logTailMaxBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}

	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		return ""
	}
	// Drop the partial first line produced by seeking into the middle.
	if start > 0 {
		if cut := bytes.IndexByte(data, '\n'); cut >= 0 && cut+1 < len(data) {
			data = data[cut+1:]
		}
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > logTailMaxLines {
		lines = lines[len(lines)-logTailMaxLines:]
	}
	return strings.Join(lines, " | ")
}
