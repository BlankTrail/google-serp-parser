module github.com/blanktrail/google-serp-parser

// The floor is a Go line that still receives security patches. Go supports the
// two newest releases, so a floor left behind them means every release build
// carries a standard library nobody is fixing any more — a property of the
// binary people download, not merely of the developer's machine.
go 1.26.0

// The patch to build with, which is a separate question from the floor above.
// The floor names a language version; left on its own it is also the toolchain
// Go fetches for anyone whose installed one is older — so a build from source
// would come out carrying 1.26.0's standard library, holes and all. Naming the
// patch here is what makes "go build" produce a patched binary on a machine
// that has never heard of this project. A newer local toolchain is used as it
// is; this only sets the bottom.
toolchain go1.26.7

require (
	github.com/PuerkitoBio/goquery v1.12.0
	golang.org/x/net v0.58.0
	golang.org/x/sys v0.47.0
	modernc.org/sqlite v1.57.0
)

require (
	github.com/andybalholm/cascadia v1.3.4 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	modernc.org/libc v1.75.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
