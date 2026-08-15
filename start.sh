#!/bin/sh
# Launch gserp from the directory this script lives in, building it first when
# only the source is present.
set -e
cd "$(dirname "$0")"

if [ -x "./gserp" ]; then
    GSERP=./gserp
elif command -v go >/dev/null 2>&1; then
    echo "gserp not found - building it from source, this takes a moment..."
    go build -o gserp ./cmd/gserp
    GSERP=./gserp
else
    echo "Neither the gserp binary nor Go was found here." >&2
    echo "Download a release build, or install Go and run: go build -o gserp ./cmd/gserp" >&2
    exit 1
fi

"$GSERP" doctor
