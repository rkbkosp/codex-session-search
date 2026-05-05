//go:build windows

package main

func processRunning(pid int) bool {
	return pid > 0
}
