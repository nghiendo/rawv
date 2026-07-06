//go:build !windows

package main

func startSystray(onExit func()) {
	// No-op for macOS/Linux to allow compilation
	// Simply block indefinitely to keep the process running
	select {}
}
