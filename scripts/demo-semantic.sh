#!/usr/bin/env bash
#
# demo-semantic.sh — LIVE proof of M5 SEMANTIC relevance: a rule whose body
# shares NO WORD with the query still injects, because the query's general
# concept ("database") semantically covers the rule's specific term
# ("PostgreSQL"/"MySQL") — while an unrelated query STILL injects nothing.
#
# This is the e2e finding M5 fixes. The M2 floor needs a SHARED WORD; the rule
#   "Always use PostgreSQL, never MySQL."   (NO word "database" in the body)
# did NOT fire for "what database should I use?". M5's curated concept-expansion
# table (postgresql/mysql → database/sql/rdbms, applied at index-build time)
# closes the gap with NO CGO, NO ONNX, NO model download — and provably does NOT
# regress the no-false-positive property (the weather query stays at ZERO).
#
# What it does:
#   1. temp vault with ONE rule whose body is EXACTLY "Always use PostgreSQL,
#      never MySQL." — deliberately NO occurrence of the word "database".
#   2. fake echo-upstream that prints the request body it received.
#   3. oikosd serve at the DEFAULT match floor (0.60 — NOT lowered) so this is a
#      real floor-clearing semantic match, not a floor-disabled pass-through.
#   4. (A) send "what database should I use for my app?" → the rule IS injected
#      (semantic: database ↔ PostgreSQL) even with no shared word.
#   5. (B) send "what is the weather today?" → ZERO injection (no false positive).
#
# Needs only: bash, curl, Go toolchain. No python required (python only pretty-
# prints if present). Pure-Go, CGO-free.
#
# Usage:  scripts/demo-semantic.sh
set -euo pipefail

export GOROOT="${GOROOT:-$(go env GOROOT 2>/dev/null)}"
export GOPATH="${GOPATH:-$HOME/go}"
export PATH="$GOROOT/bin:$PATH"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

WORK="$(mktemp -d)"
UPSTREAM_PORT=18097
OIKOS_ADDR="127.0.0.1:4141"

cleanup() {
  [[ -n "${OIKOS_PID:-}" ]] && kill "$OIKOS_PID" 2>/dev/null || true
  [[ -n "${UP_PID:-}" ]] && kill "$UP_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

pp() { # pretty-print JSON if python3 is around, else raw
  if command -v python3 >/dev/null 2>&1; then python3 -m json.tool; else cat; fi
}

echo "==> [1/5] temp vault — ONE rule, body has NO word 'database'"
VAULT="$WORK/vault"
mkdir -p "$VAULT"
cat > "$VAULT/use-postgres.md" <<'EOF'
---
id: use-postgres
title: Use Postgres
kind: guardrail
weight: 0.95
confidence: 0.95
status: live
---
Always use PostgreSQL, never MySQL.
EOF
echo "    rule body: 'Always use PostgreSQL, never MySQL.'  (note: NO 'database')"
echo

echo "==> [2/5] fake echo-upstream"
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
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(os.Stderr, "FORWARDED_REQUEST_BODY:%s\n", body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"echo","choices":[{"message":{"role":"assistant","content":"(echo)"}}]}`))
	})
	if err := http.ListenAndServe(addr, nil); err != nil {
		panic(err)
	}
}
EOF
go build -o "$WORK/upstream" "$WORK/upstream.go"
"$WORK/upstream" "127.0.0.1:$UPSTREAM_PORT" 2> "$WORK/upstream.log" &
UP_PID=$!
echo "    fake upstream pid=$UP_PID on 127.0.0.1:$UPSTREAM_PORT"
echo

echo "==> [3/5] oikosd at the DEFAULT floor (0.60 — a REAL semantic clearance)"
go build -o "$WORK/oikosd" ./cmd/oikos
OIKOS_VAULT="$VAULT" \
OIKOS_UPSTREAM_BASE="http://127.0.0.1:$UPSTREAM_PORT" \
  "$WORK/oikosd" serve > "$WORK/oikosd.log" 2>&1 &
OIKOS_PID=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://$OIKOS_ADDR/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
echo "    /health: $(curl -fsS "http://$OIKOS_ADDR/health")"
echo

echo "==> [4/5] (A) SEMANTIC match — query 'database', rule says 'PostgreSQL'"
SEM='{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use for my app?"}]}'
echo "    client sent: $SEM"
BEFORE_N="$(grep -ac '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" || true)"
curl -fsS "http://$OIKOS_ADDR/v1/chat/completions" -H 'Content-Type: application/json' -d "$SEM" >/dev/null
for _ in $(seq 1 50); do
  NOW_N="$(grep -ac '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" || true)"
  if [[ "${NOW_N:-0}" -gt "${BEFORE_N:-0}" ]]; then break; fi
  sleep 0.05
done
FWD_SEM="$(grep -a '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" | tail -1 | sed 's/^FORWARDED_REQUEST_BODY://')"
echo "    upstream received:"; echo "----"; echo "$FWD_SEM" | pp; echo "----"
if echo "$FWD_SEM" | grep -q '<!-- oikos:rules:begin v=1 -->' && \
   echo "$FWD_SEM" | grep -q 'Always use PostgreSQL, never MySQL.'; then
  echo "RESULT (A): PASS — the rule fired for a query with NO shared word"
  echo "            (semantic: database ↔ PostgreSQL), at the default 0.60 floor."
else
  echo "RESULT (A): FAIL — the semantic rule was NOT injected."
  echo "  oikosd.log:"; sed 's/^/    /' "$WORK/oikosd.log"
  exit 1
fi
echo

echo "==> [5/5] (B) IRRELEVANT — query 'weather' must STILL inject NOTHING"
IRR='{"model":"gpt-4o","messages":[{"role":"user","content":"what is the weather today?"}]}'
echo "    client sent: $IRR"
BEFORE_N="$(grep -ac '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" || true)"
curl -fsS "http://$OIKOS_ADDR/v1/chat/completions" -H 'Content-Type: application/json' -d "$IRR" >/dev/null
for _ in $(seq 1 50); do
  NOW_N="$(grep -ac '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" || true)"
  if [[ "${NOW_N:-0}" -gt "${BEFORE_N:-0}" ]]; then break; fi
  sleep 0.05
done
FWD_IRR="$(grep -a '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" | tail -1 | sed 's/^FORWARDED_REQUEST_BODY://')"
echo "    upstream received:"; echo "----"; echo "$FWD_IRR" | pp; echo "----"
if echo "$FWD_IRR" | grep -q '<!-- oikos:rules:begin'; then
  echo "RESULT (B): FAIL — weather query got an oikos block (semantic over-fired)."
  exit 1
elif [[ "$FWD_IRR" == "$IRR" ]]; then
  echo "RESULT (B): PASS — weather forwarded VERBATIM, ZERO injection."
  echo
  echo "M5 PROVEN: semantic match fires (database↔PostgreSQL) WITHOUT regressing"
  echo "           the no-false-positive moat (weather stays at zero)."
  exit 0
else
  echo "RESULT (B): FAIL — forwarded body was not the verbatim original:"
  echo "    $FWD_IRR"
  exit 1
fi
