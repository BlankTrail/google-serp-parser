#!/bin/sh
# Build every release artefact into dist/: the binaries, the archives around
# them, and the checksums over the lot.
#
# This script is what the release workflow runs. It is a script rather than a
# list of steps in the workflow so that the artefacts can be built again by
# hand, on any machine, and compared against what was published — a binary
# nobody but a CI runner can reproduce is a binary taken on trust.
#
# Usage:
#   scripts/dist.sh [version]
#
# The version is stamped into the binary and is what "gserp version" prints.
# Left out, it is "dev", which is the right answer for a build that is not a
# release.
#
# GSERP_INTEGRATION_KEY, if set, names this build to BlankTrail's cabinet so a
# licence bought because of the parser is credited to it. It is deliberately not
# a default: an unset key stamps nothing, which is what a build by anybody other
# than this project should do. See cmd/gserp/integration.go.
set -eu

cd "$(dirname "$0")/.."

VERSION="${1:-dev}"
MODULE=github.com/blanktrail/google-serp-parser

# -s -w drop the symbol and DWARF tables, which no user of a release build
# reads and which are a third of its size. -trimpath takes the building
# machine's directory names out of the binary: without it every panic names
# somebody's home directory, and two people building the same source get two
# different files.
LDFLAGS="-s -w -X $MODULE/internal/version.current=$VERSION"
if [ -n "${GSERP_INTEGRATION_KEY:-}" ]; then
	LDFLAGS="$LDFLAGS -X $MODULE/internal/version.integrationKey=$GSERP_INTEGRATION_KEY"
fi

OUT=dist
STAGE="$OUT/stage"
# The staging area is cleared and the output is not: a second run overwrites the
# artefacts it made last time, while a half-built staging directory left by a
# run that failed would otherwise be packed into an archive.
rm -rf "$STAGE"
mkdir -p "$OUT" "$STAGE"

# One line per artefact: the platform, the architecture, the name the bare
# binary is published under (a dash where none is), and the archive.
#
# Windows is published twice over. The bare .exe is the whole program and the
# thing the README tells a reader to download — one file, nothing to unpack.
# The zip beside it is for whoever wants the READMEs next to it. It carries no
# start.sh, because on Windows the program opens the browser itself.
TARGETS="windows amd64 gserp.exe gserp-windows-amd64.zip
windows arm64 gserp-arm64.exe gserp-windows-arm64.zip
linux amd64 - gserp-linux-amd64.tar.gz
linux arm64 - gserp-linux-arm64.tar.gz
darwin amd64 - gserp-macos-intel.tar.gz
darwin arm64 - gserp-macos-apple-silicon.tar.gz"

echo "$TARGETS" | while read -r goos goarch bare archive; do
	stem="${archive%.tar.gz}"
	stem="${stem%.zip}"
	dir="$STAGE/$stem"
	mkdir -p "$dir"

	exe=gserp
	[ "$goos" = windows ] && exe=gserp.exe

	echo "building $goos/$goarch"
	# CGO off is what makes one runner able to build all six: the SQLite driver
	# is pure Go, so nothing here needs a cross-compiler.
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags "$LDFLAGS" -o "$dir/$exe" ./cmd/gserp

	cp README.md README.ru.md LICENSE "$dir/"
	[ "$goos" = windows ] || cp start.sh "$dir/"

	if [ "$bare" != "-" ]; then
		cp "$dir/$exe" "$OUT/$bare"
	fi

	case "$archive" in
	*.zip)
		# Flat, so double-clicking the zip and dragging the .exe out gives a
		# program that runs rather than one folder deep.
		(cd "$dir" && zip -q -X "../../$archive" ./*)
		;;
	*.tar.gz)
		# Nested in a directory of its own, so unpacking in a home directory
		# leaves one folder rather than five loose files.
		(cd "$STAGE" && tar czf "../$archive" "$stem")
		;;
	esac
done

rm -rf "$STAGE"

# Checksums last, over everything published, in the order the release notes list
# it. The asterisk is what sha256sum writes for a file read as binary, and is
# what "sha256sum -c" expects to read back.
(
	cd "$OUT"
	rm -f SHA256SUMS.txt
	for f in gserp.exe gserp-arm64.exe gserp-windows-amd64.zip gserp-windows-arm64.zip \
		gserp-linux-amd64.tar.gz gserp-linux-arm64.tar.gz \
		gserp-macos-apple-silicon.tar.gz gserp-macos-intel.tar.gz; do
		if command -v sha256sum >/dev/null 2>&1; then
			sha256sum -b "$f"
		else
			# macOS has shasum instead, and writes the same two columns the
			# other way round.
			shasum -a 256 -b "$f" | sed 's/ \{1,\}/ /'
		fi
	done >SHA256SUMS.txt
)

echo
echo "dist/ holds:"
ls -1 "$OUT"
