// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2018 Datadog, Inc.

// +build windows

package secrets

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/DataDog/datadog-agent/pkg/util/log"
	"golang.org/x/sys/windows/registry"
)

var (
	advapi32                    = syscall.NewLazyDLL("advapi32.dll")
	procCreateProcessWithLogonW = advapi32.NewProc("CreateProcessWithLogonW")
)

const (
	// The user created at install time with low/no rights
	username             = "datadog_secretuser"
	passwordRegistryPath = "SOFTWARE\\Datadog\\Datadog Agent\\secrets"
)

func checkRights(path string) error {
	log.Warn("checkRights not yet implemented on windows")
	return nil
}

func getPasswordFromRegistry() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		passwordRegistryPath,
		registry.READ)
	if err != nil {
		if err == registry.ErrNotExist {
			return "", fmt.Errorf("Secret user password does not found in the registry")
		}
		return "", fmt.Errorf("can't read secrets user password from registry: %s", err)
	}
	defer k.Close()

	password, _, err := k.GetStringValue(username)
	if err != nil {
		return "", fmt.Errorf("Could not read password for secrets user from registry: %s", err)
	}
	return password, nil
}

func skipStdinCopyError(err error) bool {
	// Ignore ERROR_BROKEN_PIPE and ERROR_NO_DATA errors copying
	// to stdin if the program completed successfully otherwise.
	// See Issue 20445.
	const _ERROR_NO_DATA = syscall.Errno(0xe8)
	pe, ok := err.(*os.PathError)
	return ok &&
		pe.Op == "write" && pe.Path == "|1" &&
		(pe.Err == syscall.ERROR_BROKEN_PIPE || pe.Err == _ERROR_NO_DATA)
}

func setInputPipe(r io.Reader, goroutine *[]func() error, closeAfterStart, closeAfterWait *[]*os.File) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %s", err)
	}

	*goroutine = append(*goroutine, func() error {
		_, err := io.Copy(pw, r)
		if skipStdinCopyError(err) {
			err = nil
		}
		if err1 := pw.Close(); err == nil {
			err = err1
		}
		return err
	})
	*closeAfterStart = append(*closeAfterStart, pr)
	*closeAfterWait = append(*closeAfterWait, pw)
	return nil
}

func setOutputPipe(w io.Writer, goroutine *[]func() error, closeAfterStart, closeAfterWait *[]*os.File) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create pipe: %s", err)
	}

	*goroutine = append(*goroutine, func() error {
		_, err := io.Copy(w, pr)
		pr.Close() // in case io.Copy stopped due to write error
		return err
	})
	*closeAfterStart = append(*closeAfterStart, pw)
	*closeAfterWait = append(*closeAfterWait, pr)
	return nil
}

// makeCmdLine builds a command line out of args by escaping "special"
// characters and joining the arguments with spaces.
func makeCmdLine(cmd string, args []string) string {
	for _, v := range args {
		cmd += " " + syscall.EscapeArg(v)
	}
	return cmd
}

func startProcess(argv0 string, argv []string, attr *syscall.ProcAttr) (*os.Process, error) {
	argv0p, err := syscall.UTF16PtrFromString(argv0)
	if err != nil {
		return nil, fmt.Errorf("can't convert command string to UTF16: %s", err)
	}

	cmdline := makeCmdLine(argv0, argv)

	argvp, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, fmt.Errorf("can't convert cmdline string to UTF16: %s", err)
	}

	// Acquire the fork lock so that no other threads
	// create new fds that are not yet close-on-exec
	// before we fork.
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	p, _ := syscall.GetCurrentProcess()
	fd := make([]syscall.Handle, len(attr.Files))
	for i := range attr.Files {
		if attr.Files[i] > 0 {
			err := syscall.DuplicateHandle(p,
				syscall.Handle(attr.Files[i]),
				p,
				&fd[i],
				0,
				true,
				syscall.DUPLICATE_SAME_ACCESS,
			)
			if err != nil {
				return nil, fmt.Errorf("can't call DuplicateHandle to execute secretBackendCommand: %s\n", err)
			}
			defer syscall.CloseHandle(syscall.Handle(fd[i]))
		}
	}

	si := new(syscall.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = syscall.STARTF_USESTDHANDLES
	si.StdInput = fd[0]
	si.StdOutput = fd[1]
	si.StdErr = fd[2]

	pi := new(syscall.ProcessInformation)

	password, err := getPasswordFromRegistry()
	if err != nil {
		return nil, err
	}

	pUsername, _ := syscall.UTF16PtrFromString("datadog_secretuser")
	pPassword, _ := syscall.UTF16PtrFromString(password)
	pLocalDB, _ := syscall.UTF16PtrFromString(".")
	flags := syscall.CREATE_UNICODE_ENVIRONMENT

	res, _, err := procCreateProcessWithLogonW.Call(
		uintptr(unsafe.Pointer(pUsername)),
		uintptr(unsafe.Pointer(pLocalDB)), // local account database
		uintptr(unsafe.Pointer(pPassword)),
		0, // logon flags
		uintptr(unsafe.Pointer(argv0p)),
		uintptr(unsafe.Pointer(argvp)),
		uintptr(flags),
		uintptr(unsafe.Pointer(nil)), // let windows load datadog_secretuser env from it's profile
		uintptr(unsafe.Pointer(nil)), // current dir: same as the one from the datadog_agent
		uintptr(unsafe.Pointer(si)),
		uintptr(unsafe.Pointer(pi)),
	)

	if res == 0 {
		return nil, fmt.Errorf("error from CreateProcessWithLogonW: %s\n", err)
	}

	// the 'handle' attribute from os.Process is private so even if we have
	// the info in 'pi.Process' we need to use 'FindProcess' to be able to
	// return a os.Process struct (which avoid us duplicating even more code
	// from the os package).
	proc, err := os.FindProcess(int(pi.ProcessId))
	if err != nil {
		return nil, fmt.Errorf("error finding backend process: %s\n", err)
	}

	return proc, nil
}

func closeFileList(fileList []*os.File) {
	for _, f := range fileList {
		f.Close()
	}
}

func execCommand(inputPayload string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(secretBackendTimeout)*time.Second)
	defer cancel()

	stdin := strings.NewReader(inputPayload)
	stdout := limitBuffer{
		buf: &bytes.Buffer{},
		max: secretBackendOutputMaxSize,
	}
	stderr := limitBuffer{
		buf: &bytes.Buffer{},
		max: secretBackendOutputMaxSize,
	}
	goroutine := []func() error{}
	closeAfterStart := []*os.File{}
	closeAfterWait := []*os.File{}

	// creating pipes for stdin, stdout and stderr
	if err := setInputPipe(stdin, &goroutine, &closeAfterStart, &closeAfterWait); err != nil {
		return nil, err
	}
	if err := setOutputPipe(stdout, &goroutine, &closeAfterStart, &closeAfterWait); err != nil {
		return nil, err
	}
	if err := setOutputPipe(stderr, &goroutine, &closeAfterStart, &closeAfterWait); err != nil {
		return nil, err
	}

	fds := []uintptr{}
	for _, fd := range closeAfterStart {
		fds = append(fds, fd.Fd())
	}

	cmd := []string{secretBackendCommand}
	cmd = append(cmd, secretBackendArguments...)
	process, err := startProcess(
		secretBackendCommand,
		secretBackendArguments,
		&syscall.ProcAttr{Files: fds},
	)
	if err != nil {
		closeFileList(closeAfterStart)
		closeFileList(closeAfterWait)
		return nil, err
	}
	closeFileList(closeAfterStart)

	// start read/write goroutines
	errch := make(chan error, len(goroutine))
	for _, fn := range goroutine {
		go func(fn func() error) {
			errch <- fn()
		}(fn)
	}

	waitDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			process.Kill()
		case <-waitDone:
		}
	}()

	state, err := process.Wait()
	close(waitDone)

	var copyError error
	for range goroutine {
		if err := <-errch; err != nil && copyError == nil {
			copyError = err
		}
	}

	closeFileList(closeAfterWait)

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("error while running '%s': command timeout", secretBackendCommand)
		}
		return nil, fmt.Errorf("error while running '%s': %s", secretBackendCommand, err)
	} else if !state.Success() {
		return nil, fmt.Errorf("'%s' exited with failure status", secretBackendCommand)
	}
	return stdout.buf.Bytes(), nil
}
