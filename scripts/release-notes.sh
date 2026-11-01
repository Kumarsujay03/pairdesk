#!/bin/sh
# Print the CHANGELOG.md section for a version (e.g. v0.1.2 or 0.1.2).
set -eu
v="${1#v}"
awk -v v="$v" '
  /^## \[/ { if (on) exit; on = ($0 ~ "^## \\[" v "\\]") ; next }
  on { print }
' CHANGELOG.md | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
