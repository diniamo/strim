package log

import (
	"fmt"
	"os"
)

func Note(a ...any) {
	NoteColor.Fprintln(os.Stderr, a...)
}

func Notef(format string, a ...any) {
	NoteColor.Fprintf(os.Stderr, format, a...)
	fmt.Fprintln(os.Stderr)
}

func Success(a ...any) {
	SuccessColor.Fprintln(os.Stderr, a...)
}

func Successf(format string, a ...any) {
	SuccessColor.Fprintf(os.Stderr, format, a...)
	fmt.Fprintln(os.Stderr)
}

func Warn(a ...any) {
	WarnColor.Fprintln(os.Stderr, a...)
}

func Warnf(format string, a ...any) {
	WarnColor.Fprintf(os.Stderr, format, a...)
	fmt.Fprintln(os.Stderr)
}

func Error(a ...any) {
	ErrorColor.Fprintln(os.Stderr, a...)
}

func Errorf(format string, a ...any) {
	ErrorColor.Fprintf(os.Stderr, format, a...)
	fmt.Fprintln(os.Stderr)
}

func Fatal(a ...any) {
	FatalColor.Fprintln(os.Stderr, a...)
	os.Exit(1)
}

func Fatalf(format string, a ...any) {
	FatalColor.Fprintf(os.Stderr, format, a...)
	fmt.Fprintln(os.Stderr)
	os.Exit(1)
}
