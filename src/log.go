package main

import (
	"fmt"
	"os"

	"github.com/LordOfTrident/ansi-go"
)

func Print(a ...any) {
	fmt.Fprint(os.Stderr, fmt.Sprint(a...))
}
func Println(a ...any) {
	fmt.Fprintln(os.Stderr, fmt.Sprint(a...))
}

func Note(a ...any) {
	s := fmt.Sprintln(a...)
	Println(ansi.Dim, s[:len(s) - 1], ansi.Reset)
}
func Notef(f string, a ...any) {
	Println(ansi.Dim, fmt.Sprintf(f, a...), ansi.Reset)
}

func Success(a ...any) {
	s := fmt.Sprintln(a...)
	Println(ansi.GreenFg, s[:len(s) - 1], ansi.Reset)
}
func Successf(f string, a ...any) {
	Println(ansi.GreenFg, fmt.Sprintf(f, a...), ansi.Reset)
}

func Warn(a ...any) {
	s := fmt.Sprintln(a...)
	Println(ansi.YellowFg, s[:len(s) - 1], ansi.Reset)
}
func Warnf(f string, a ...any) {
	Println(ansi.YellowFg, fmt.Sprintf(f, a...), ansi.Reset)
}

func Error(a ...any) {
	s := fmt.Sprintln(a...)
	Println(ansi.RedFg, s[:len(s) - 1], ansi.Reset)
}
func Errorf(f string, a ...any) {
	Println(ansi.RedFg, fmt.Sprintf(f, a...), ansi.Reset)
}

func Fatal(a ...any) {
	s := fmt.Sprintln(a...)
	Println(ansi.RedFg + ansi.Bold, s[:len(s) - 1], ansi.Reset)
	TrapExit(1)
}
func Fatalf(f string, a ...any) {
	Println(ansi.RedFg + ansi.Bold, fmt.Sprintf(f, a...), ansi.Reset)
	TrapExit(1)
}
