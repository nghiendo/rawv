//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

//go:embed icon.ico
var embeddedIcon []byte

type HANDLE uintptr
type HWND HANDLE
type HINSTANCE HANDLE
type HICON HANDLE
type HMENU HANDLE

const (
	WM_CREATE    = 0x0001
	WM_DESTROY   = 0x0002
	WM_COMMAND   = 0x0111
	WM_USER      = 0x0400
	WM_TRAY_MSG  = WM_USER + 1
	WM_RBUTTONUP = 0x0205
	WM_LBUTTONUP = 0x0202

	NIM_ADD    = 0x0000
	NIM_MODIFY = 0x0001
	NIM_DELETE = 0x0002

	NIF_MESSAGE = 0x0001
	NIF_ICON    = 0x0002
	NIF_TIP     = 0x0004

	MF_STRING    = 0x0000
	MF_SEPARATOR = 0x0800
	MF_CHECKED   = 0x0008
	MF_UNCHECKED = 0x0000

	IDI_INFORMATION = 32516

	HKEY_CURRENT_USER = 0x80000001
	KEY_SET_VALUE     = 0x0002
	KEY_QUERY_VALUE   = 0x0001
	REG_SZ            = 1
)

type WNDCLASSEXW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   HINSTANCE
	Icon       HICON
	Cursor     HANDLE
	Background HANDLE
	MenuName   *uint16
	ClassName  *uint16
	IconSm     HICON
}

type POINT struct {
	X int32
	Y int32
}

type MSG struct {
	HWnd    HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

type NOTIFYICONDATAW struct {
	Size            uint32
	HWnd            HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            HICON
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	TimeoutOrVersion uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        [16]byte
	BalloonIcon     HICON
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pLoadIconW        = user32.NewProc("LoadIconW")
	pLoadImageW       = user32.NewProc("LoadImageW")
	pCheckMenuItem    = user32.NewProc("CheckMenuItem")

	pCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	pAppendMenuW         = user32.NewProc("AppendMenuW")
	pTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	pGetCursorPos        = user32.NewProc("GetCursorPos")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")

	pShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	pRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	pRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	pRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	pRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	pRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

var (
	wndProcCallback = syscall.NewCallback(wndProc)
	globalHWnd      HWND
	globalMenu      HMENU
	onExitFunc      func()
)

func wndProc(hWnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_CREATE:
		return 0
	case WM_TRAY_MSG:
		switch lParam {
		case WM_RBUTTONUP, WM_LBUTTONUP:
			var pt POINT
			pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

			pSetForegroundWindow.Call(hWnd)

			pTrackPopupMenu.Call(
				uintptr(globalMenu),
				0x0000|0x0002, // TPM_LEFTALIGN | TPM_RIGHTBUTTON
				uintptr(pt.X),
				uintptr(pt.Y),
				0,
				hWnd,
				0,
			)
		}
		return 0
	case WM_COMMAND:
		cmdID := wParam & 0xffff
		if cmdID == 1001 { // Exit clicked
			pDestroyWindow.Call(hWnd)
		} else if cmdID == 1002 { // Autostart toggle clicked
			enabled := isAutostartEnabled()
			setAutostart(!enabled)

			newFlags := uintptr(MF_STRING)
			if !enabled {
				newFlags |= MF_CHECKED
			}
			pCheckMenuItem.Call(uintptr(globalMenu), 1002, newFlags)
		}
		return 0
	case WM_DESTROY:
		// Delete system tray icon
		nid := NOTIFYICONDATAW{
			HWnd: HWND(hWnd),
			ID:   1,
		}
		nid.Size = uint32(unsafe.Sizeof(nid))
		pShellNotifyIconW.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))

		pPostQuitMessage.Call(0)
		if onExitFunc != nil {
			onExitFunc()
		}
		return 0
	default:
		r, _, _ := pDefWindowProcW.Call(hWnd, msg, wParam, lParam)
		return r
	}
}

func startSystray(onExit func()) {
	onExitFunc = onExit

	hInstance, _, _ := pGetModuleHandleW.Call(0)

	className, _ := syscall.UTF16PtrFromString("GoDownloaderTrayClass")

	wc := WNDCLASSEXW{
		Style:     0,
		WndProc:   wndProcCallback,
		Instance:  HINSTANCE(hInstance),
		ClassName: className,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))

	r, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if r == 0 {
		return
	}

	windowName, _ := syscall.UTF16PtrFromString("GoDownloaderTrayWindow")
	hWnd, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0,
		0, 0, 0, 0,
		0, // Hidden window message receiver
		0,
		hInstance,
		0,
	)
	if hWnd == 0 {
		return
	}
	globalHWnd = HWND(hWnd)

	// Create Popup Menu
	menu, _, _ := pCreatePopupMenu.Call()
	globalMenu = HMENU(menu)

	statusStr, _ := syscall.UTF16PtrFromString("Status: Active")
	pAppendMenuW.Call(uintptr(globalMenu), MF_STRING|0x0001, 1000, uintptr(unsafe.Pointer(statusStr))) // MF_GRAYED = 0x0001

	// Start with Windows checkbox item
	autostartFlags := uintptr(MF_STRING)
	if isAutostartEnabled() {
		autostartFlags |= MF_CHECKED
	}
	autostartStr, _ := syscall.UTF16PtrFromString("Start with Windows")
	pAppendMenuW.Call(uintptr(globalMenu), autostartFlags, 1002, uintptr(unsafe.Pointer(autostartStr)))

	pAppendMenuW.Call(uintptr(globalMenu), MF_SEPARATOR, 0, 0)

	exitStr, _ := syscall.UTF16PtrFromString("Exit")
	pAppendMenuW.Call(uintptr(globalMenu), MF_STRING, 1001, uintptr(unsafe.Pointer(exitStr)))

	// Load icon: write embedded icon to temp file then load it
	// (LoadImageW from resource requires LR_LOADFROMFILE, not LR_SHARED)
	var icon HANDLE
	icoPath := loadIconToTemp()
	if icoPath != "" {
		icoPathUTF16, _ := syscall.UTF16PtrFromString(icoPath)
		// IMAGE_ICON=1, LR_LOADFROMFILE=0x0010, LR_DEFAULTSIZE=0x0040
		h, _, _ := pLoadImageW.Call(
			0,
			uintptr(unsafe.Pointer(icoPathUTF16)),
			1,
			0, 0,
			0x0010|0x0040,
		)
		if h != 0 {
			icon = HANDLE(h)
		}
		os.Remove(icoPath) // clean up temp file
	}
	if icon == 0 {
		hIcon, _, _ := pLoadIconW.Call(0, IDI_INFORMATION)
		icon = HANDLE(hIcon)
	}

	// Register System Tray Icon
	nid := NOTIFYICONDATAW{
		HWnd:            globalHWnd,
		ID:              1,
		Flags:           NIF_MESSAGE | NIF_ICON | NIF_TIP,
		CallbackMessage: WM_TRAY_MSG,
		Icon:            HICON(icon),
	}
	nid.Size = uint32(unsafe.Sizeof(nid))
	copy(nid.Tip[:], utf16FromString("Go Downloader"))

	pShellNotifyIconW.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))

	// Message Loop
	var msg MSG
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func isAutostartEnabled() bool {
	subKey, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Run`)
	valueName, _ := syscall.UTF16PtrFromString("GoDownloader")

	var hKey syscall.Handle
	r, _, _ := pRegOpenKeyExW.Call(
		HKEY_CURRENT_USER,
		uintptr(unsafe.Pointer(subKey)),
		0,
		KEY_QUERY_VALUE,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if r != 0 {
		return false
	}
	defer pRegCloseKey.Call(uintptr(hKey))

	var buf [512]uint16
	var size uint32 = uint32(len(buf) * 2)
	r, _, _ = pRegQueryValueExW.Call(
		uintptr(hKey),
		uintptr(unsafe.Pointer(valueName)),
		0,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	return r == 0
}

func setAutostart(enabled bool) error {
	path, err := os.Executable()
	if err != nil {
		return err
	}

	subKey, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Run`)
	valueName, _ := syscall.UTF16PtrFromString("GoDownloader")

	var hKey syscall.Handle

	if enabled {
		r, _, _ := pRegOpenKeyExW.Call(
			HKEY_CURRENT_USER,
			uintptr(unsafe.Pointer(subKey)),
			0,
			KEY_SET_VALUE,
			uintptr(unsafe.Pointer(&hKey)),
		)
		if r != 0 {
			return fmt.Errorf("failed to open registry key: %d", r)
		}
		defer pRegCloseKey.Call(uintptr(hKey))

		cmd := fmt.Sprintf(`"%s" -server`, path)
		cmdUTF16, _ := syscall.UTF16FromString(cmd)

		r, _, _ = pRegSetValueExW.Call(
			uintptr(hKey),
			uintptr(unsafe.Pointer(valueName)),
			0,
			REG_SZ,
			uintptr(unsafe.Pointer(&cmdUTF16[0])),
			uintptr(len(cmdUTF16)*2),
		)
		if r != 0 {
			return fmt.Errorf("failed to set registry value: %d", r)
		}
	} else {
		r, _, _ := pRegOpenKeyExW.Call(
			HKEY_CURRENT_USER,
			uintptr(unsafe.Pointer(subKey)),
			0,
			KEY_SET_VALUE,
			uintptr(unsafe.Pointer(&hKey)),
		)
		if r != 0 {
			return fmt.Errorf("failed to open registry key: %d", r)
		}
		defer pRegCloseKey.Call(uintptr(hKey))

		pRegDeleteValueW.Call(
			uintptr(hKey),
			uintptr(unsafe.Pointer(valueName)),
		)
	}

	return nil
}

func utf16FromString(s string) []uint16 {
	u, _ := syscall.UTF16FromString(s)
	res := make([]uint16, 128)
	copy(res, u)
	return res
}

// loadIconToTemp writes the embedded icon to a temp file and returns its path.
// The caller is responsible for deleting the temp file.
func loadIconToTemp() string {
	if len(embeddedIcon) == 0 {
		return ""
	}
	f, err := os.CreateTemp("", "rawv-icon-*.ico")
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Write(embeddedIcon); err != nil {
		os.Remove(f.Name())
		return ""
	}
	return f.Name()
}
