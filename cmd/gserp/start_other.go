//go:build !windows

// SPDX-License-Identifier: MIT

package main

import "context"

// noArguments prints the usage. Elsewhere this program is started from a shell,
// and a shell is where somebody who typed its name with nothing after it is
// asking what it does.
func noArguments(context.Context) error {
	usage()
	return nil
}
