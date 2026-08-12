package main

import (
	"errors"
	"fmt"
	"os"
)

// kelson process exit codes — the CI contract for `kelson diff` (issue #46).
//
//	0  no changes
//	1  usage or runtime error
//	2  changes present
//	3  a blocker: an enforce-mode PolicyViolation, or an Unvalidated resource
//	   whose prerequisite is genuinely absent (InBatch false)
//
// Codes 2 and 3 are *expected* outcomes, not failures, so they must not print
// cobra's spurious "Error:" line. rootError() centralises error reporting so
// newRootCmd can set SilenceErrors and main() owns both the message and the
// exit status.
const (
	exitOK   = 0
	exitErr  = 1
	exitDiff = 2
	exitBlk  = 3
)

// exitError is a command outcome that is valid but non-zero. Unlike a real
// error (exit 1) it carries an empty message, so main() exits with its code
// without printing a spurious "Error:" line.
type exitError struct {
	code int
}

func (e *exitError) Error() string { return "" }

// resolveExit maps an Execute() error to (stderr message, exit code). A nil
// error is exit 0 with no message. A plain error is exit 1 with its message.
// An *exitError is the code it carries with no message.
func resolveExit(err error) (string, int) {
	if err == nil {
		return "", exitOK
	}
	var xe *exitError
	if errors.As(err, &xe) {
		return "", xe.code
	}
	return err.Error(), exitErr
}

// rootError reports an Execute() error the way cobra would — "Error: <msg>" on
// stderr — and returns the exit code. The diff command's codes 2 and 3 need
// the string branch skipped, which is why newRootCmd silences cobra's own
// error printing and defers it here.
func rootError(err error) int {
	msg, code := resolveExit(err)
	if msg != "" {
		fmt.Fprintln(os.Stderr, "Error:", msg)
	}
	return code
}
