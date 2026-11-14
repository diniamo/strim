package main

import (
	"fmt"

	"github.com/LordOfTrident/ansi-go"
)

func Note(a ...any) {
	s := fmt.Sprintln(a...)
	ansi.Eprintln(ansi.Dim, s[:len(s) - 1])
}
func Notef(f string, a ...any) {
	ansi.Eprintln(ansi.Dim, fmt.Sprintf(f, a...))
}

func Success(a ...any) {
	s := fmt.Sprintln(a...)
	ansi.Eprintln(ansi.GreenFg, s[:len(s) - 1])
}
func Successf(f string, a ...any) {
	ansi.Eprintln(ansi.GreenFg, fmt.Sprintf(f, a...))
}

func Warn(a ...any) {
	s := fmt.Sprintln(a...)
	ansi.Eprintln(ansi.YellowFg, s[:len(s) - 1])
}
func Warnf(f string, a ...any) {
	ansi.Eprintln(ansi.YellowFg, fmt.Sprintf(f, a...))
}

func Error(a ...any) {
	s := fmt.Sprintln(a...)
	ansi.Eprintln(ansi.RedFg, s[:len(s) - 1])
}
func Errorf(f string, a ...any) {
	ansi.Eprintln(ansi.RedFg, fmt.Sprintf(f, a...))
}

func Fatal(a ...any) {
	s := fmt.Sprintln(a...)
	ansi.Eprintln(ansi.RedFg + ansi.Bold, s[:len(s) - 1])
	TrapExit(1)
}
func Fatalf(f string, a ...any) {
	ansi.Eprintln(ansi.RedFg + ansi.Bold, fmt.Sprintf(f, a...))
	TrapExit(1)
}
