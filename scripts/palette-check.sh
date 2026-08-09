#!/usr/bin/env bash
#
# Every mermaid diagram in this repository draws from one palette, defined
# in the legend table of docs/architecture.md. Nothing stops a diagram
# using a colour that is not in it, and nothing notices when a palette
# change lands in six diagrams out of seven, so this does.
#
# Two checks:
#   1. every hex colour used by any mermaid block appears in the legend
#   2. the %%{init}%% theme directives are byte identical to each other
#
# The second exists because sequence diagrams cannot take classDef, so the
# four in request-lifecycle.md re-encode the palette by hand. They are the
# copies most likely to drift.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

LEGEND="docs/architecture.md"
status=0

# The legend block is the table rows carrying backticked hex values, plus
# the text colour sentence beneath it. Anything outside "How to read the
# diagrams" is a diagram, not a definition.
legend_hexes="$(
  sed -n '/^## How to read the diagrams$/,/^## /p' "$LEGEND" \
    | grep -oiE '#[0-9a-f]{6}' \
    | tr 'A-F' 'a-f' \
    | sort -u
)"

if [ -z "$legend_hexes" ]; then
  echo "palette-check: no colours found in $LEGEND, has the legend moved?" >&2
  exit 1
fi

# Every markdown file that can carry a diagram.
files="$(git ls-files '*.md')"

used_hexes="$(
  for f in $files; do
    awk '/^```mermaid$/{inb=1; next} /^```$/{inb=0} inb' "$f"
  done | grep -oiE '#[0-9a-f]{6}' | tr 'A-F' 'a-f' | sort -u
)"

for hex in $used_hexes; do
  if ! printf '%s\n' "$legend_hexes" | grep -qx "$hex"; then
    echo "palette-check: $hex is used by a diagram but is not in the $LEGEND legend" >&2
    status=1
  fi
done

# The theme directives, which carry the palette for sequence diagrams.
directives="$(
  for f in $files; do
    grep -h '^%%{init:' "$f" 2>/dev/null || true
  done
)"

if [ -n "$directives" ]; then
  distinct="$(printf '%s\n' "$directives" | sort -u | wc -l)"
  if [ "$distinct" -ne 1 ]; then
    echo "palette-check: the %%{init}%% theme directives are not identical ($distinct variants)." >&2
    echo "  They re-encode the palette by hand for sequence diagrams, so they must agree." >&2
    status=1
  fi
fi

if [ "$status" -eq 0 ]; then
  count="$(printf '%s\n' "$used_hexes" | grep -c . || true)"
  echo "palette-check: ok, $count colours all defined in the legend"
fi

exit "$status"
