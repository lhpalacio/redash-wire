#!/bin/sh
# Generates THIRD_PARTY_LICENSES.md: the verbatim license and copyright notices
# for every Go module statically linked into the redash-wire binary. GoReleaser
# runs this before building archives; run it locally with `make licenses`.
#
# Test-only dependencies (e.g. go-sql-driver/mysql, which is MPL-2.0) are
# intentionally excluded because they are not distributed in the binary.
set -eu

PKG="./cmd/redash-wire"
OUT="${1:-THIRD_PARTY_LICENSES.md}"

go mod download

# Modules compiled into the binary, minus the standard library and our own module.
mods=$(go list -deps "$PKG" \
  | xargs go list -f '{{with .Module}}{{.Path}}{{end}}' \
  | grep -v '^$' \
  | grep -v '^github.com/lhpalacio/redash-wire' \
  | sort -u)

{
  echo "# Third-party licenses"
  echo
  echo "redash-wire is distributed under the MIT License (see LICENSE). Its binaries"
  echo "statically link the Go modules below; each module's license and copyright"
  echo "notice is reproduced verbatim, as those licenses require. Regenerate this file"
  echo "with \`make licenses\`."
} > "$OUT"

count=0
missing=""
for mod in $mods; do
  dir=$(go list -m -f '{{.Dir}}' "$mod")
  ver=$(go list -m -f '{{.Version}}' "$mod")
  files=$(find "$dir" -maxdepth 1 -type f \
    \( -iname 'license*' -o -iname 'licence*' -o -iname 'copying*' -o -iname 'notice*' \) \
    | sort)
  if [ -z "$files" ]; then
    missing="$missing $mod"
    continue
  fi
  {
    echo
    echo "---"
    echo
    echo "## $mod $ver"
    for f in $files; do
      echo
      echo '```'
      cat "$f"
      echo '```'
    done
  } >> "$OUT"
  count=$((count + 1))
done

if [ -n "$missing" ]; then
  echo "ERROR: no license file found for:$missing" >&2
  exit 1
fi

echo "Wrote $OUT ($count modules)"
