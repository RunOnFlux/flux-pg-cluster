// Package log provides a minimal structured logger that mimics the shell-script
// log style so existing tooling can still grep logs by timestamp/section.
package log

import (
	"fmt"
	"os"
	"time"
)

func ts() string { return time.Now().Format("2006-01-02 15:04:05") }

func Section(title string) {
	bar := "================================================================================"
	fmt.Fprintln(os.Stdout, bar)
	fmt.Fprintln(os.Stdout, title)
	fmt.Fprintln(os.Stdout, bar)
}

func Infof(format string, a ...any) {
	fmt.Fprintf(os.Stdout, "%s: %s\n", ts(), fmt.Sprintf(format, a...))
}

func Warnf(format string, a ...any) {
	fmt.Fprintf(os.Stdout, "%s: WARNING: %s\n", ts(), fmt.Sprintf(format, a...))
}

func Errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s: ERROR: %s\n", ts(), fmt.Sprintf(format, a...))
}

func Fatalf(format string, a ...any) {
	Errorf(format, a...)
	os.Exit(1)
}
