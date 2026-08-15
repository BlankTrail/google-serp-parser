// SPDX-License-Identifier: MIT

package version

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile reads a file from the repository root, two levels above this package.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestStartBat_KeepsItsWindowOpenOnFailure(t *testing.T) {
	// The whole point of the .bat is that it is double-clicked. A failing run
	// that closes its own window instantly tells the user nothing at all.
	bat := repoFile(t, "start.bat")
	if !strings.Contains(strings.ToLower(bat), "pause") {
		t.Error("start.bat never pauses, so a failure would vanish with the window")
	}
	if !strings.Contains(bat, "gserp") {
		t.Error("start.bat does not launch gserp")
	}
}

func TestStartBat_UsesCRLF(t *testing.T) {
	// cmd.exe silently drops the last line of an LF-only batch file.
	bat := repoFile(t, "start.bat")
	if strings.Contains(bat, "\n") && !strings.Contains(bat, "\r\n") {
		t.Error("start.bat has LF line endings; cmd.exe needs CRLF")
	}
}

func TestStartSh_IsStrictAndRunsFromItsOwnDirectory(t *testing.T) {
	sh := repoFile(t, "start.sh")
	if !strings.Contains(sh, "set -e") {
		t.Error("start.sh does not use set -e, so it would carry on after a failed step")
	}
	if !strings.Contains(sh, "cd ") {
		t.Error("start.sh does not cd to its own directory; launching it from elsewhere would break")
	}
	if !strings.Contains(sh, "gserp") {
		t.Error("start.sh does not launch gserp")
	}
}
