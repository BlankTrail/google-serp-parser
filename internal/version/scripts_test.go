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
