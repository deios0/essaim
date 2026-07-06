#!/usr/bin/env bash
#
# demo-learn.sh — LIVE proof of essaim M3: essaim LEARNS BACK from an exchange and
# EMITS the ranked rules into a native CLAUDE.md.
#
# Two demonstrations:
#   (A) LEARNS-BACK — drive two exchanges through the real essaimd proxy:
#         1. a T0 SIGIL turn:  "/remember Always use PostgreSQL, never MySQL"
#            → a NEW status:active rule file appears under  vault/remembered/<date>/
#         2. a T1 HEURISTIC correction turn (a stated preference)
#            → a NEW status:draft rule file appears under  vault/_inbox/
#       The capture runs OFF the response path; the client stream is verbatim.
#   (B) EMITTER — run the NativeFileEmitter against a temp CLAUDE.md and print
#       the ranked, fenced essaim block it wrote (LIVE-only).
#
# It needs only: bash, curl, and the Go toolchain. Pure-Go, CGO-free, no network.
#
# Usage:  scripts/demo-learn.sh
set -uo pipefail

export GOROOT="${GOROOT:-$(go env GOROOT 2>/dev/null)}"
export GOPATH="${GOPATH:-$HOME/go}"
export PATH="$GOROOT/bin:$PATH"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

WORK="$(mktemp -d)"
UPSTREAM_PORT=18097
ESSAIM_ADDR="127.0.0.1:4141"

cleanup() {
  [[ -n "${ESSAIM_PID:-}" ]] && kill "$ESSAIM_PID" 2>/dev/null || true
  [[ -n "${UP_PID:-}" ]] && kill "$UP_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

VAULT="$WORK/vault"
mkdir -p "$VAULT"

echo "==> [1/6] seed the vault with ONE live rule (so the emitter has something to emit)"
cat > "$VAULT/use-postgres.md" <<'EOF'
---
id: use-postgres
title: Use Postgres
kind: guardrail
weight: 0.95
confidence: 0.95
status: live
---
Always use the PostgreSQL database, never MySQL.
EOF
echo "    vault: $VAULT"
echo

echo "==> [2/6] fake streaming upstream (returns a normal assistant SSE answer)"
cat > "$WORK/upstream.go" <<'EOF'
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	addr := os.Args[1]
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// A short streamed answer; the capture tap reassembles it off-path.
		for _, chunk := range []string{"Understood", ", noted."} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", chunk)
			if fl != nil { fl.Flush() }
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil { fl.Flush() }
	})
	_ = http.ListenAndServe(addr, nil)
}
EOF
go build -o "$WORK/upstream" "$WORK/upstream.go" || { echo "build upstream failed"; exit 1; }
"$WORK/upstream" "127.0.0.1:$UPSTREAM_PORT" 2>"$WORK/upstream.log" &
UP_PID=$!
echo "    upstream pid=$UP_PID on 127.0.0.1:$UPSTREAM_PORT"
echo

echo "==> [3/6] build + start essaimd (vault + capture/learning loop active)"
go build -o "$WORK/essaimd" ./cmd/essaim || { echo "build essaimd failed"; exit 1; }
ESSAIM_VAULT="$VAULT" \
ESSAIM_UPSTREAM_BASE="http://127.0.0.1:$UPSTREAM_PORT" \
ESSAIM_MATCH_FLOOR=0.0 \
  "$WORK/essaimd" serve > "$WORK/essaimd.log" 2>&1 &
ESSAIM_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "http://$ESSAIM_ADDR/health" >/dev/null 2>&1 && break
  sleep 0.1
done
echo "    essaimd pid=$ESSAIM_PID; /health: $(curl -fsS "http://$ESSAIM_ADDR/health")"
echo

wait_for_md() {  # $1 = dir, $2 = label
  for _ in $(seq 1 100); do
    f="$(find "$1" -name '*.md' 2>/dev/null | head -1)"
    [[ -n "$f" ]] && { echo "$f"; return 0; }
    sleep 0.1
  done
  return 1
}

echo "==> [4/6] (A) LEARNS-BACK #1 — T0 SIGIL turn through the proxy"
SIGIL_REQ='{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"/remember Always use PostgreSQL, never MySQL"}]}'
echo "    client sent: $SIGIL_REQ"
curl -fsS "http://$ESSAIM_ADDR/v1/chat/completions" -H 'Content-Type: application/json' -d "$SIGIL_REQ" >/dev/null
ACTIVE_FILE="$(wait_for_md "$VAULT/remembered" remembered)"
if [[ -z "${ACTIVE_FILE:-}" ]]; then
  echo "RESULT: FAIL — no active rule appeared under remembered/"; sed 's/^/  /' "$WORK/essaimd.log"; exit 1
fi
echo "    >>> NEW learned rule file: $ACTIVE_FILE"
echo "    ------------------------------------------------------------------"
sed 's/^/    /' "$ACTIVE_FILE"
echo "    ------------------------------------------------------------------"
echo

echo "==> [5/6] (A) LEARNS-BACK #2 — T1 HEURISTIC correction → DRAFT in _inbox/"
T1_REQ='{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"always prefer composition over inheritance because it is the team rule"}]}'
echo "    client sent: $T1_REQ"
curl -fsS "http://$ESSAIM_ADDR/v1/chat/completions" -H 'Content-Type: application/json' -d "$T1_REQ" >/dev/null
DRAFT_FILE="$(wait_for_md "$VAULT/_inbox" _inbox)"
if [[ -z "${DRAFT_FILE:-}" ]]; then
  echo "RESULT: FAIL — no draft rule appeared under _inbox/"; sed 's/^/  /' "$WORK/essaimd.log"; exit 1
fi
echo "    >>> NEW draft rule file in _inbox/: $DRAFT_FILE"
echo "    ------------------------------------------------------------------"
sed 's/^/    /' "$DRAFT_FILE"
echo "    ------------------------------------------------------------------"
echo "    /health (capture meters): $(curl -fsS "http://$ESSAIM_ADDR/health")"
echo

echo "==> [6/6] (B) STANDALONE EMITTER — \`essaim emit\` writes AGENTS.md with NO proxy"
# Kill the proxy first to PROVE the emit path is fully standalone (no daemon).
kill "$ESSAIM_PID" 2>/dev/null || true
ESSAIM_PID=""
TARGET="$WORK/CLAUDE.md"
cat > "$TARGET" <<'EOF'
# My Project

Some of the user's own instructions live here. essaim must NEVER touch them.
EOF
# The SAME binary, the first-class standalone command — no throwaway program.
echo "    running: essaim emit --vault \$VAULT --file claude-code=\$TARGET   (proxy stopped)"
ESSAIM_CONFIG="$WORK/empty-config.json" \
  "$WORK/essaimd" emit --vault "$VAULT" --file "claude-code=$TARGET"
EMIT_RC=$?
if [[ $EMIT_RC -ne 0 ]]; then echo "RESULT: FAIL — standalone emit run failed"; exit 1; fi
echo "    >>> the standalone emitter wrote this CLAUDE.md (user content preserved, ranked block fenced):"
echo "    =================================================================="
sed 's/^/    /' "$TARGET"
echo "    =================================================================="
echo

# Verdict.
if grep -q 'Always use the PostgreSQL database, never MySQL.' "$TARGET" \
   && grep -q '<!-- essaim:rules:begin' "$TARGET" \
   && grep -q "Some of the user's own instructions" "$TARGET" \
   && [[ -n "${ACTIVE_FILE:-}" && -n "${DRAFT_FILE:-}" ]]; then
  echo "RESULT: PASS — essaim LEARNED a sigil rule (remembered/) AND a T1 draft (_inbox/)"
  echo "        from live exchanges, AND \`essaim emit\` wrote the ranked LIVE block into"
  echo "        CLAUDE.md with the PROXY STOPPED (standalone), preserving the user's content."
  exit 0
else
  echo "RESULT: FAIL — see output above."
  exit 1
fi
