//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

type BROWSEINFOW struct {
	HWndOwner      HWND
	PidlRoot       uintptr
	DisplayName    *uint16
	Title          *uint16
	Flags          uint32
	Callback       uintptr
	LParam         uintptr
	Image          int32
}

var (
	ole32 = syscall.NewLazyDLL("ole32.dll")

	pSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	pSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	pCoInitialize         = ole32.NewProc("CoInitialize")
	pCoUninitialize       = ole32.NewProc("CoUninitialize")
	pCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

func selectFolderDialog() (string, error) {
	// Initialize COM library for the thread
	pCoInitialize.Call(0)
	defer pCoUninitialize.Call()

	title, _ := syscall.UTF16PtrFromString("Select Save Directory")
	var displayName [260]uint16

	// BIF_RETURNONLYFSDIRS = 0x0001, BIF_USENEWUI = 0x0040 (New layout)
	bi := BROWSEINFOW{
		HWndOwner:   globalHWnd,
		DisplayName: &displayName[0],
		Title:       title,
		Flags:       0x0001 | 0x0040,
	}

	pidl, _, _ := pSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", nil // User cancelled
	}
	defer pCoTaskMemFree.Call(pidl)

	var path [260]uint16
	r, _, _ := pSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if r == 0 {
		return "", fmt.Errorf("failed to get path from folder selection")
	}

	return syscall.UTF16ToString(path[:]), nil
}
