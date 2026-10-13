#!/bin/sh
# SPDX-License-Identifier: Apache-2.0

set -eu

# Run the host-built go-licenses tool with the same GOOS, GOARCH and CGO_ENABLED
# as the provider build. Build-only and test-only dependencies are excluded.
output=${1:?provide an absolute output directory}
case "$output" in
  /*) ;;
  *) echo "license output directory must be absolute" >&2; exit 1 ;;
esac
"${GO_LICENSES:-go-licenses}" save ./cmd/provider \
  --ignore github.com/mistralai/nvsentinel-provider-mistral \
  --save_path="$output"

# go-licenses omits the standard library. Preserve its license and any additional
# notices along the source paths of used packages, including Go's vendored code.
# Also retain package-level notices that accompany a module-level license.
dependencies=$(mktemp)
trap 'rm -f "$dependencies"' EXIT HUP INT TERM
go_root=$(go env GOROOT)
go list -deps -f '{{if .Standard}}{{.Dir}}||go{{else if .Module}}{{.Dir}}|{{.Module.Dir}}|{{.Module.Path}}{{end}}' \
  ./cmd/provider > "$dependencies"

while IFS='|' read -r source root name; do
  [ -n "$source" ] || continue
  [ "$name" != github.com/mistralai/nvsentinel-provider-mistral ] || continue
  if [ "$name" = go ]; then root=$go_root; fi
  while :; do
    case "$source" in
      "$root"|"$root"/*) ;;
      *) echo "dependency source is outside its module" >&2; exit 1 ;;
    esac
    for notice in "$source"/[Ll][Ii][Cc][Ee][Nn][SsCc][Ee]* \
      "$source"/[Nn][Oo][Tt][Ii][Cc][Ee]* \
      "$source"/[Cc][Oo][Pp][Yy][Ii][Nn][Gg]* \
      "$source"/[Cc][Oo][Pp][Yy][Rr][Ii][Gg][Hh][Tt]*; do
      [ -f "$notice" ] || continue
      # A package may define a Go type in license.go; it is not a notice.
      case "$notice" in *.go) continue ;; esac
      relative=${notice#"$root"/}
      destination="$output/$name/$relative"
      [ ! -f "$destination" ] || continue
      mkdir -p "$(dirname "$destination")"
      cp "$notice" "$destination"
    done
    [ "$source" != "$root" ] || break
    source=$(dirname "$source")
  done
done < "$dependencies"

test -s "$output/go/LICENSE"
