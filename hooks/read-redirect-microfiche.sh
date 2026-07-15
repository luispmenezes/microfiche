#!/bin/bash
# PreToolUse hook on Read: deny full-file reads of large prose/log files
# and redirect to the microfiche MCP tool (~3-4x fewer input tokens).
#
# Escape hatches (always allowed):
#   - ranged reads (offset/limit/pages) — the sanctioned path for
#     byte-exact strings and edit flows
#   - images / PDFs / notebooks (Read has dedicated handling)
#   - exactness-heavy data formats (json, csv, lockfiles, minified...) —
#     these are all exact values, the worst content to read from pixels
#   - files below the win threshold or above the ingestion ceiling
#
# NOTE: this hook assumes the registered microfiche server's density
# profile matches your model (fable profile <-> Fable models). Running
# the dense fable profile on Opus-tier models degrades exact recall —
# re-register with `-profile opus` if you switch.

THRESHOLD_BYTES=51200   # 50KB — below this, text is cheap (prompt cache)
CEILING_BYTES=204800    # 200KB — above this, full-file ingestion of ANY
                        # kind loses to Grep + ranged reads

input=$(cat)
fp=$(jq -r '.tool_input.file_path // empty' <<<"$input")

[ -z "$fp" ] && exit 0

# Targeted/partial reads are the sanctioned path for exact strings and edits
has_range=$(jq -r '.tool_input | (has("offset") or has("limit") or has("pages"))' <<<"$input")
[ "$has_range" = "true" ] && exit 0

shopt -s nocasematch
case "$fp" in
  # formats Read handles natively
  *.png|*.jpg|*.jpeg|*.gif|*.webp|*.bmp|*.ico|*.pdf|*.ipynb) exit 0 ;;
  # exactness-heavy data formats — never image these
  *.json|*.jsonl|*.ndjson|*.csv|*.tsv|*.xml|*.svg|*.yaml|*.yml|*.toml|*.lock|*.map|*.min.js|*.min.css) exit 0 ;;
esac
shopt -u nocasematch

[ -f "$fp" ] || exit 0

size=$(stat -f%z "$fp" 2>/dev/null || stat -c%s "$fp" 2>/dev/null)
[ -z "$size" ] && exit 0
[ "$size" -lt "$THRESHOLD_BYTES" ] && exit 0

kb=$((size / 1024))

if [ "$size" -gt "$CEILING_BYTES" ]; then
  # too big for full-file ingestion by ANY method — teach the targeted move
  jq -n --arg reason "This file is ${kb}KB — too large to ingest whole by any method. Use Grep to locate the region you need, then either Read with offset/limit or call mcp__microfiche__microfiche with line_start/line_end for that slice." \
    '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$reason}}'
  exit 0
fi

jq -n --arg reason "This file is ${kb}KB. Read it with the mcp__microfiche__microfiche tool instead (file_path: $fp) — it costs ~3-4x fewer input tokens. Exception: if you need byte-exact strings from it or are about to Edit it, call Read again with offset/limit to fetch only the slice you need; ranged reads are allowed." \
  '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$reason}}'
