//go:build windows

// SPDX-License-Identifier: MIT

package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tray.ico is the icon the notification area shows.
//
// It is embedded and handed to the system as bytes rather than compiled into
// the executable's resources, because resources need a step between the source
// and the binary and this program is built by "go build" and nothing else.
//
//go:embed tray.ico
var trayIcon []byte

// What the menu offers. There are two things somebody wants from a server
// running behind an icon — look at it, and stop it — and a menu with more than
// that on it is one they have to read.

// trayClassName is the kind of window the icon talks through, and the name
// anything outside this process asks the system for to know the program is up.
const trayClassName = "gserp-tray"

const (
	menuOpen = 1
	menuStop = 2
)

// The Windows calls this needs. They are looked up lazily, so a build for
// another system never mentions them and this file is the only place that knows
// they exist.
var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassEx      = user32.NewProc("RegisterClassExW")
	procCreateWindowEx       = user32.NewProc("CreateWindowExW")
	procDefWindowProc        = user32.NewProc("DefWindowProcW")
	procGetMessage           = user32.NewProc("GetMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessage      = user32.NewProc("DispatchMessageW")
	procPostQuitMessage      = user32.NewProc("PostQuitMessage")
	procDestroyWindow        = user32.NewProc("DestroyWindow")
	procCreatePopupMenu      = user32.NewProc("CreatePopupMenu")
	procAppendMenu           = user32.NewProc("AppendMenuW")
	procDestroyMenu          = user32.NewProc("DestroyMenu")
	procTrackPopupMenu       = user32.NewProc("TrackPopupMenu")
	procGetCursorPos         = user32.NewProc("GetCursorPos")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procCreateIconFromResEx  = user32.NewProc("CreateIconFromResourceEx")
	procShowWindow           = user32.NewProc("ShowWindow")
	procFindWindow           = user32.NewProc("FindWindowW")
	procGetModuleHandle      = kernel32.NewProc("GetModuleHandleW")
	procShellNotifyIcon      = shell32.NewProc("Shell_NotifyIconW")
	procGetConsoleWindow     = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessLst = kernel32.NewProc("GetConsoleProcessList")
)

const (
	wmDestroy     = 0x0002
	wmCommand     = 0x0111
	wmTrayMessage = 0x0400 + 1 // WM_APP + 1

	wmRButtonUp   = 0x0205
	wmLButtonDown = 0x0201

	nimAdd     = 0x0000
	nimDelete  = 0x0002
	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004

	mfString = 0x0000

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002

	swHide = 0
)

// notifyIconData is what the notification area is told about the icon. The
// layout is the system's and the field order cannot be rearranged.
type notifyIconData struct {
	Size             uint32
	Wnd              windows.Handle
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	VersionOrTimeout uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUIDItem         windows.GUID
	BalloonIcon      windows.Handle
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type point struct{ X, Y int32 }

type msg struct {
	Wnd     windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// startedFromExplorer reports whether this program was double-clicked rather
// than typed at a prompt.
//
// It asks how many processes share this console: a window opened for this
// program alone holds one, and a prompt somebody typed into holds that shell as
// well. It is the only way to tell the two apart, and it is what decides
// whether running with no arguments prints the usage or serves the interface —
// somebody at a prompt asked what the program does, somebody who
// double-clicked asked for the program.
func startedFromExplorer() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessLst.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// hideConsole takes away the black window that opens with a double-click.
//
// It is hidden rather than freed: freeing the console would take standard error
// with it, and what this program writes there is the one account of what went
// wrong that survives to be read.
func hideConsole() {
	wnd, _, _ := procGetConsoleWindow.Call()
	if wnd != 0 {
		_, _, _ = procShowWindow.Call(wnd, swHide)
	}
}

// openInBrowser asks the system to open an address the way a link is opened.
func openInBrowser(at string) {
	// rundll32 rather than "cmd /c start": start is a shell builtin, so it needs
	// a shell, and a shell opened for it flashes a window of its own.
	_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", at).Start()
}

// serveWithTray serves the interface with an icon in the notification area, and
// returns when the icon's menu says to stop or the server ends on its own.
//
// The message loop has to be on the thread the window was made on, which is why
// the server runs beside it rather than the other way about.
func serveWithTray(ctx context.Context, out io.Writer, opts serveOptions) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	opened := make(chan string, 1)
	opts.onListen = func(at string) {
		select {
		case opened <- at:
		default:
		}
	}

	done := make(chan error, 1)
	go func() { done <- serveInterface(ctx, out, opts) }()

	// The browser follows the interface rather than leading it: the socket is
	// bound before this arrives, so the tab lands on a page instead of on a
	// refusal.
	select {
	case at := <-opened:
		openInBrowser("http://" + at + "/")
		if err := runTray(ctx, stop, "http://"+at+"/"); err != nil {
			_, _ = fmt.Fprintln(out, "gserp: the notification area refused an icon:", err)
		}
	case err := <-done:
		return err
	}
	return <-done
}

// runTray puts the icon up and runs the message loop until the menu says to
// stop or the server ends.
func runTray(ctx context.Context, stop context.CancelFunc, at string) error {
	icon, err := loadEmbeddedIcon()
	if err != nil {
		return err
	}

	// A window belongs to the thread that made it and its messages arrive on
	// that thread's queue and nowhere else. Without this the goroutine is free to
	// be moved between making the window and asking for its messages, and then
	// the icon is up and pressing it does nothing at all.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var wnd windows.Handle
	proc := func(h windows.Handle, message uint32, wp, lp uintptr) uintptr {
		switch message {
		case wmTrayMessage:
			switch lp {
			case wmRButtonUp, wmLButtonDown:
				showMenu(h, at, stop)
			}
		case wmCommand:
			switch wp & 0xffff {
			case menuOpen:
				openInBrowser(at)
			case menuStop:
				stop()
				_, _, _ = procDestroyWindow.Call(uintptr(h))
			}
		case wmDestroy:
			_, _, _ = procPostQuitMessage.Call(0)
		}
		r, _, _ := procDefWindowProc.Call(uintptr(h), uintptr(message), wp, lp)
		return r
	}

	class, err := registerTrayClass(proc)
	if err != nil {
		return err
	}
	wnd, err = createTrayWindow(class)
	if err != nil {
		return err
	}

	data, err := putIconUp(wnd, icon, "gserp — "+at)
	if err != nil {
		_, _, _ = procDestroyWindow.Call(uintptr(wnd))
		return err
	}
	defer func() {
		// Taking the icon down is the last thing this does and there is nobody
		// left to tell if it fails: the window it belongs to is going with it.
		takeIconDown(data)
	}()

	// A server that ends on its own — a port already taken, a history that will
	// not open — must take the icon with it, or the icon outlives what it stands
	// for.
	go func() {
		<-ctx.Done()
		_, _, _ = procDestroyWindow.Call(uintptr(wnd))
	}()

	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return nil
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// registerTrayClass registers the kind of window the icon talks through.
//
// The class belongs to this program's module, which makes it this program's
// class and nobody else's: a class registered under a module is looked up under
// that module alone, so the window is not something another program can reach
// by name, and nothing here means it to be.
func registerTrayClass(proc func(windows.Handle, uint32, uintptr, uintptr) uintptr) (*uint16, error) {
	class := windows.StringToUTF16Ptr(trayClassName)
	// The module this program is. There is no resource module to name in a
	// program built by "go build" and nothing else, so this is the process
	// itself, and the class belongs to it rather than to everything running.
	module, _, _ := procGetModuleHandle.Call(0)
	cls := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   windows.NewCallback(proc),
		Instance:  windows.Handle(module),
		ClassName: class,
	}
	// A class already registered is the same class: this runs once in a program
	// and more than once under a test, and the second registration failing is not
	// a window that cannot be made.
	if r, _, e := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&cls))); r == 0 {
		if !errors.Is(e, windows.ERROR_CLASS_ALREADY_EXISTS) {
			return nil, fmt.Errorf("registering the window class: %w", e)
		}
	}
	return class, nil
}

// createTrayWindow makes the window itself. It is never shown: it exists to
// receive what the notification area sends when the icon is pressed.
func createTrayWindow(class *uint16) (windows.Handle, error) {
	module, _, _ := procGetModuleHandle.Call(0)
	h, _, e := procCreateWindowEx.Call(0, uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(class)), 0, 0, 0, 0, 0, 0, 0, module, 0)
	if h == 0 {
		return 0, fmt.Errorf("making the window the icon talks to: %w", e)
	}
	return windows.Handle(h), nil
}

// putIconUp asks the notification area for a place, and gives back what has to
// be handed back to take it away again.
//
// This is the step that decides whether anybody sees this program at all: the
// window above it is invisible by design, so a refusal here is a server running
// with nothing on the screen to reach it by.
func putIconUp(wnd, icon windows.Handle, tip string) (*notifyIconData, error) {
	data := &notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             wnd,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayMessage,
		Icon:            icon,
	}
	// The tip is a fixed room the system reads to its own end, so a longer
	// address is cut rather than written past what was given.
	copy(data.Tip[:len(data.Tip)-1], windows.StringToUTF16(tip))
	if r, _, e := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(data))); r == 0 {
		return nil, fmt.Errorf("putting the icon up: %w", e)
	}
	return data, nil
}

// takeIconDown gives the place back.
func takeIconDown(data *notifyIconData) {
	_, _, _ = procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(data)))
}

// showMenu draws the two things somebody wants from a server behind an icon.
func showMenu(wnd windows.Handle, at string, stop context.CancelFunc) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer func() { _, _, _ = procDestroyMenu.Call(menu) }()

	_, _, _ = procAppendMenu.Call(menu, mfString, menuOpen,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Open the interface"))))
	_, _, _ = procAppendMenu.Call(menu, mfString, menuStop,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Stop the server"))))

	var p point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	// Without this the menu stays up after the pointer leaves it, which is the
	// one thing everybody notices about a badly made tray icon.
	_, _, _ = procSetForegroundWindow.Call(uintptr(wnd))
	_, _, _ = procTrackPopupMenu.Call(menu, tpmLeftAlign|tpmRightButton,
		uintptr(p.X), uintptr(p.Y), 0, uintptr(wnd), 0)
	_ = at
	_ = stop
}

// loadEmbeddedIcon hands the system the icon's own bytes.
//
// An .ico file is a small directory of images and then the images themselves;
// what the system wants here is one image, so the directory is read for where
// it starts. Only the first is used: this icon has one, and a file with several
// would be a file somebody replaced with something this does not promise to
// understand.
func loadEmbeddedIcon() (windows.Handle, error) {
	const header, entry = 6, 16
	if len(trayIcon) < header+entry {
		return 0, errors.New("the icon is too short to be one")
	}
	size := binary.LittleEndian.Uint32(trayIcon[header+8:])
	offset := binary.LittleEndian.Uint32(trayIcon[header+12:])
	if uint64(offset)+uint64(size) > uint64(len(trayIcon)) {
		return 0, errors.New("the icon says it is larger than it is")
	}
	image := trayIcon[offset : offset+size]

	h, _, e := procCreateIconFromResEx.Call(
		uintptr(unsafe.Pointer(&image[0])), uintptr(len(image)),
		1,          // an icon rather than a cursor
		0x00030000, // the version every icon since Windows 3 carries
		0, 0, 0)
	if h == 0 {
		return 0, fmt.Errorf("reading the icon: %w", e)
	}
	return windows.Handle(h), nil
}
