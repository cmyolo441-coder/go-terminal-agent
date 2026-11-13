#!/bin/sh
# GTA plugin slash command (executable kind). GTA shows its stdout to the
# user; nothing is sent to the model. Words typed after /today arrive as
# "$@", and the raw text is also in $GTA_ARGS.

echo "Date: $(date +%Y-%m-%d)"
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null) || branch="(not a git repository)"
echo "Branch: $branch"
if [ -n "$GTA_ARGS" ]; then
  echo "Arguments: $GTA_ARGS"
fi
