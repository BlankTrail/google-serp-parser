# Commands for working on gserp. Run "just" on its own to list them.
#
# The release artefacts are built by scripts/dist.sh rather than by a recipe
# here, because the release workflow runs that same script: one implementation
# means what CI publishes and what you can build at your desk cannot drift.

[private]
default:
    @just --list

# Build gserp for this machine
build:
    go build -trimpath -o gserp ./cmd/gserp

# Build the Windows desktop version (tray icon, no console) for this machine's architecture
build-desktop:
    #!/bin/sh
    set -eu
    # Windows is the desktop build: it is the one that hides the console, puts
    # an icon in the notification area and opens the browser by itself. The
    # architecture follows this machine's rather than being asked for, so the
    # command works the same on an Intel box and an ARM one.
    case "$(uname -m)" in
    arm64 | aarch64) arch=arm64 ;;
    *) arch=amd64 ;;
    esac
    echo "building windows/$arch"
    CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
        go build -trimpath -o gserp.exe ./cmd/gserp

# Run the tests, with the race detector
test:
    go test -race ./...

# Vet, lint, and check the dependencies for known holes
lint:
    go vet ./...
    go vet -tags live ./...
    gofmt -l .
    golangci-lint run
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# Build every release artefact into dist/ (version defaults to "dev")
dist version="dev":
    ./scripts/dist.sh {{ version }}
