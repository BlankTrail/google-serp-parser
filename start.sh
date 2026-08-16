#!/bin/sh
# Open gserp's browser interface from the directory this script lives in,
# building the program first when only the source is present.
set -e
cd "$(dirname "$0")"

# Where the interface listens, and so where the browser is sent. It is one
# value rather than two so the address printed by the program and the address
# opened for the reader cannot drift apart.
: "${GSERP_ADDR:=127.0.0.1:8080}"

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

# The browser follows the interface rather than leading it. The socket is bound
# before the program opens anything else, so a browser that waits a moment lands
# on a page rather than on a refusal.
open_browser() {
    sleep 2
    for opener in xdg-open open sensible-browser; do
        if command -v "$opener" >/dev/null 2>&1; then
            "$opener" "http://$GSERP_ADDR/" >/dev/null 2>&1 &
            return
        fi
    done
    echo "No browser could be opened here. Open http://$GSERP_ADDR/ yourself."
}
open_browser &

echo "Opening http://$GSERP_ADDR/ once the interface is listening."
exec "$GSERP" serve -addr "$GSERP_ADDR"
