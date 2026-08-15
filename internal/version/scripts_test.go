// SPDX-License-Identifier: MIT

package version

import (
	"os"
	"path/filepath"
	"regexp"
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

// exitOnFailureRe matches every failure exit in start.bat. It is intentionally
// loose about whitespace/case so re-formatting the .bat does not break the test.
var exitOnFailureRe = regexp.MustCompile(`(?i)exit\s*/b\s*1`)

// pausedExitOnFailureRe matches only the failure exits that are immediately
// preceded by a pause (allowing for indentation and CRLF/LF line endings).
var pausedExitOnFailureRe = regexp.MustCompile(`(?i)pause[ \t]*\r?\n[ \t]*exit\s*/b\s*1`)

func TestStartBat_KeepsItsWindowOpenOnFailure(t *testing.T) {
	// The whole point of the .bat is that it is double-clicked. A failing run
	// that closes its own window instantly tells the user nothing at all. It
	// is not enough for the word "pause" to appear anywhere in the file (a
	// .bat that only pauses on success would satisfy that) - each of the
	// three failure exits must pause immediately before it.
	bat := repoFile(t, "start.bat")
	if !strings.Contains(bat, "gserp") {
		t.Error("start.bat does not launch gserp")
	}

	exits := exitOnFailureRe.FindAllString(bat, -1)
	if len(exits) == 0 {
		t.Fatal("start.bat has no `exit /b 1` failure path to check")
	}
	pausedExits := pausedExitOnFailureRe.FindAllString(bat, -1)
	if len(pausedExits) != len(exits) {
		t.Errorf("start.bat has %d failure exit(s) but only %d are immediately preceded by pause; "+
			"every `exit /b 1` must pause first or the window closes before the error can be read",
			len(exits), len(pausedExits))
	}
}

func TestStartBat_UsesCRLF(t *testing.T) {
	// cmd.exe silently drops the last line of an LF-only batch file. Checking
	// that "\r\n" appears somewhere is not enough - a file with mixed CRLF and
	// bare LF is just as broken, so strip every properly paired CRLF and make
	// sure nothing is left over.
	bat := repoFile(t, "start.bat")
	stripped := strings.ReplaceAll(bat, "\r\n", "")
	if strings.Contains(stripped, "\n") {
		t.Error("start.bat has a bare LF not paired with a CR; cmd.exe needs CRLF on every line, not just most of them")
	}
}

func TestStartSh_IsStrictAndRunsFromItsOwnDirectory(t *testing.T) {
	sh := repoFile(t, "start.sh")
	if !strings.Contains(sh, "set -e") {
		t.Error("start.sh does not use set -e, so it would carry on after a failed step")
	}
	// Check the specific form the script uses, not just any "cd " substring -
	// an unrelated cd elsewhere in the file would otherwise satisfy the test
	// without the script actually running from its own directory.
	if !strings.Contains(sh, `cd "$(dirname "$0")"`) {
		t.Error(`start.sh does not cd to its own directory via cd "$(dirname "$0")"; launching it from elsewhere would break`)
	}
	if !strings.Contains(sh, "gserp") {
		t.Error("start.sh does not launch gserp")
	}
}
