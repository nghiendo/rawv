//go:build !windows

package main

import "fmt"

func selectFolderDialog() (string, error) {
	return "", fmt.Errorf("dialog not implemented on this platform")
}
