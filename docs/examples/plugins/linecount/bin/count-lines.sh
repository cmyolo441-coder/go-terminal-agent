#!/bin/sh
# GTA plugin tool: count the lines of one workspace file.
#
# stdin:  one JSON object, for example {"path":"README.md"}. GTA has already
#         checked it against input_schema and compacted it to one line.
# stdout: exactly one JSON object: {"output": "...", "is_error": false}
# cwd:    the workspace root.

input=$(cat)

# Minimal JSON extraction without jq. It handles ordinary paths; a path
# that contains a double quote or a backslash is refused below, which also
# keeps the reply valid JSON without an escaping step.
path=$(printf '%s' "$input" | sed -n 's/.*"path":"\([^"\\]*\)".*/\1/p')

# reply MESSAGE IS_ERROR -- MESSAGE must not contain " or \
reply() {
  printf '{"output": "%s", "is_error": %s}\n' "$1" "$2"
}

if [ -z "$path" ]; then
  reply "path is missing or contains a quote or backslash" true
  exit 0
fi

case "$path" in
  /* | .. | ../* | */../* | */..)
    reply "path must be relative to the workspace and must not contain .." true
    exit 0
    ;;
esac

if [ ! -f "$path" ]; then
  reply "no such file: $path" true
  exit 0
fi

lines=$(wc -l < "$path" | tr -d ' ')
reply "$path has $lines lines" false
