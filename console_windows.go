package main

import (
	"golang.org/x/sys/windows"
)

// The Windows build is an ordinary console program, so `status`, `link`
// and the installer's checks print like any CLI. When the logon task
// starts `run`, Windows gives it a console window of its own; hide that.
// A console shared with a terminal (more than one attached process) is
// left alone.
func hideOwnConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getWindow := kernel32.NewProc("GetConsoleWindow")
	getList := kernel32.NewProc("GetConsoleProcessList")
	user32 := windows.NewLazySystemDLL("user32.dll")
	showWindow := user32.NewProc("ShowWindow")

	hwnd, _, _ := getWindow.Call()
	if hwnd == 0 {
		return
	}
	var pids [4]uint32
	n, _, _ := getList.Call(uintptr(unsafePointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		return
	}
	const swHide = 0
	showWindow.Call(hwnd, swHide)
}
