//go:build windows

// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestTrayIcon_IsAnIconTheSystemWillTake(t *testing.T) {
	// The icon is bytes in this binary rather than a compiled resource, so
	// nothing but this checks that what was embedded is an icon at all. A file
	// replaced by something else, or an offset read wrongly, is a program that
	// serves the interface and leaves nothing in the notification area to reach
	// it by — which is the whole of what a double-click is meant to give.
	h, err := loadEmbeddedIcon()
	if err != nil {
		t.Fatalf("the embedded icon is not one the system will take: %v", err)
	}
	if h == 0 {
		t.Fatal("the system took the icon and gave nothing back")
	}
}

func TestTrayWindow_IsMadeAndCanBeFoundByTheClassItRegisters(t *testing.T) {
	// The icon talks to a window, and a window that was never made is an icon
	// that never appears. Both steps are checked here because both fail quietly:
	// the class and the window are made with no module handle, which the system
	// is free to refuse.
	// The window procedure has to hand on what it does not answer: refusing
	// WM_NCCREATE is refusing the window, and the system then says nothing about
	// why there is no window.
	class, err := registerTrayClass(func(h windows.Handle, m uint32, wp, lp uintptr) uintptr {
		r, _, _ := procDefWindowProc.Call(uintptr(h), uintptr(m), wp, lp)
		return r
	})
	if err != nil {
		t.Fatalf("registering the class the icon's window belongs to: %v", err)
	}
	wnd, err := createTrayWindow(class)
	if err != nil {
		t.Fatalf("making the window the icon talks to: %v", err)
	}
	defer func() { _, _, _ = procDestroyWindow.Call(uintptr(wnd)) }()
	if wnd == 0 {
		t.Fatal("the window was made and its handle is nothing")
	}

	// And it is a window this program can be asked for by the name it registered.
	// The class belongs to this program's module, so this is a question only this
	// process can ask and only this process is meant to.
	found, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(class)), 0)
	if found != uintptr(wnd) {
		t.Errorf("the system finds %v under the registered class, and the window made is %v", found, wnd)
	}
}

func TestTrayIcon_GoesUpInTheNotificationAreaAndComesBackDown(t *testing.T) {
	// The window the icon talks through is invisible, so this is the only step
	// whose failure a reader would ever meet: a server running and nothing on the
	// screen to open it by. It is checked against the real notification area,
	// because a refusal is what is being checked for and nothing else can refuse.
	icon, err := loadEmbeddedIcon()
	if err != nil {
		t.Fatalf("loadEmbeddedIcon: %v", err)
	}
	class, err := registerTrayClass(func(h windows.Handle, m uint32, wp, lp uintptr) uintptr {
		r, _, _ := procDefWindowProc.Call(uintptr(h), uintptr(m), wp, lp)
		return r
	})
	if err != nil {
		t.Fatalf("registerTrayClass: %v", err)
	}
	wnd, err := createTrayWindow(class)
	if err != nil {
		t.Fatalf("createTrayWindow: %v", err)
	}
	defer func() { _, _, _ = procDestroyWindow.Call(uintptr(wnd)) }()

	data, err := putIconUp(wnd, icon, "gserp — http://127.0.0.1:8080/")
	if err != nil {
		t.Fatalf("the notification area refused the icon: %v", err)
	}
	takeIconDown(data)

	// It says which window to send presses to and which message to send, and an
	// icon that says neither is one nothing happens on.
	if data.Wnd != wnd {
		t.Errorf("the icon sends presses to %v and the window made is %v", data.Wnd, wnd)
	}
	if data.CallbackMessage != wmTrayMessage {
		t.Errorf("the icon sends %#x, and what is listened for is %#x",
			data.CallbackMessage, wmTrayMessage)
	}
}

func TestTrayIcon_KeepsTheAddressWithinTheRoomTheSystemGaveForIt(t *testing.T) {
	// The tip is a fixed room the system reads to its own end. A longer address
	// written straight in leaves it with no end to find.
	long := "gserp — http://" + strings.Repeat("a", 400) + ":8080/"
	icon, err := loadEmbeddedIcon()
	if err != nil {
		t.Fatalf("loadEmbeddedIcon: %v", err)
	}
	class, err := registerTrayClass(func(h windows.Handle, m uint32, wp, lp uintptr) uintptr {
		r, _, _ := procDefWindowProc.Call(uintptr(h), uintptr(m), wp, lp)
		return r
	})
	if err != nil {
		t.Fatalf("registerTrayClass: %v", err)
	}
	wnd, err := createTrayWindow(class)
	if err != nil {
		t.Fatalf("createTrayWindow: %v", err)
	}
	defer func() { _, _, _ = procDestroyWindow.Call(uintptr(wnd)) }()

	data, err := putIconUp(wnd, icon, long)
	if err != nil {
		t.Fatalf("the notification area refused an icon with a long address: %v", err)
	}
	takeIconDown(data)
	if data.Tip[len(data.Tip)-1] != 0 {
		t.Error("the tip runs to the very end of the room, so the system reads past it")
	}
}

func TestTrayIcon_IsLaidOutTheWayTheSystemLaysItOut(t *testing.T) {
	// The layout is the system's, and 976 is what the system reads on this
	// machine's word size. It is written here as a number rather than taken from
	// the type, because taking it from the type would be the type agreeing with
	// itself: a field added, removed or moved would change both at once and
	// nothing would say so. The notification area does not always refuse a size
	// that is wrong, which is what makes this worth pinning at all.
	if got := unsafe.Sizeof(notifyIconData{}); got != 976 {
		t.Errorf("what is handed to the notification area is %d bytes, and it reads 976", got)
	}
	data := notifyIconData{Size: uint32(unsafe.Sizeof(notifyIconData{}))}
	if data.Size != 976 {
		t.Errorf("it says it is %d bytes, and it is read as 976", data.Size)
	}
}
