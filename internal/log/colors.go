package log

import "github.com/fatih/color"

var SuccessColor = color.New(color.FgGreen)
var WarnColor = color.New(color.FgYellow)
var ErrorColor = color.New(color.FgRed)
var FatalColor = color.New(color.FgRed, color.Bold)
var NoteColor = color.New(color.Faint)
