#!/usr/bin/env bash
#
# demo-injection.sh — LIVE proof that oikos injects a vault rule into a real
# POST /v1/chat/completions request before it reaches the upstream.
#
# What it does:
#   1. creates a temp vault with ONE Obsidian-style rule (.md + YAML frontmatter):
#        title "Use Postgres", body "Always use PostgreSQL, never MySQL."
#   2. starts a fake "upstream" that simply ECHOES the request body it received
#      (so we can see exactly what oikos forwarded).
#   3. starts `oikosd serve` pointed at the fake upstream + the temp vault.
#   4. sends a real POST /v1/chat/completions with the user message
#        "what database should I use?"
#   5. PRINTS the forwarded request body the upstream received — showing the
#      injected   <!-- oikos:rules:begin v=1 -->...Use PostgreSQL...<!-- oikos:rules:end -->
#      system message that the client never sent.
#
# It needs only: bash, curl, and the Go toolchain (to build oikosd + the fake
# upstream). No python, no nc, no external services. Pure-Go, CGO-free.
#
# Usage:  scripts/demo-injection.sh
set -euo pipefail

# --- Go toolchain (oikos pins 1.22; GOTOOLCHAIN=local keeps it pinned) --------
export GOROOT="${GOROOT:-$(go env GOROOT 2>/dev/null)}"
export GOPATH="${GOPATH:-$HOME/go}"
export PATH="$GOROOT/bin:$PATH"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

WORK="$(mktemp -d)"
UPSTREAM_PORT=18099           # fake upstream
OIKOS_ADDR="127.0.0.1:4141"   # oikosd's fixed loopback bind

cleanup() {
  [[ -n "${OIKOS_PID:-}" ]] && kill "$OIKOS_PID" 2>/dev/null || true
  [[ -n "${UP_PID:-}" ]] && kill "$UP_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> [1/5] temp vault with one .md rule"
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
Always use the PostgreSQL database, never MySQL.
EOF
echo "    vault: $VAULT"
echo "    rule : use-postgres.md  (title 'Use Postgres')"
echo

echo "==> [2/5] fake echo-upstream (prints the request body it receives)"
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
		// Echo the EXACT forwarded request body back to the demo, on stderr (so
		// the caller can capture it) and as the HTTP response.
		fmt.Fprintf(os.Stderr, "FORWARDED_REQUEST_BODY:%s\n", body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"echo","choices":[{"message":{"role":"assistant","content":"(fake upstream echoed your request)"}}]}`))
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

echo "==> [3/5] build + start oikosd pointed at the fake upstream + vault"
go build -o "$WORK/oikosd" ./cmd/oikos
OIKOS_VAULT="$VAULT" \
OIKOS_UPSTREAM_BASE="http://127.0.0.1:$UPSTREAM_PORT" \
OIKOS_MATCH_FLOOR=0.0 \
  "$WORK/oikosd" serve > "$WORK/oikosd.log" 2>&1 &
OIKOS_PID=$!
echo "    oikosd pid=$OIKOS_PID on http://$OIKOS_ADDR"

# Wait for oikosd to come up (poll /health).
for _ in $(seq 1 50); do
  if curl -fsS "http://$OIKOS_ADDR/health" >/dev/null 2>&1; then break; fi
  sleep 0.1
done
echo "    /health: $(curl -fsS "http://$OIKOS_ADDR/health")"
echo

echo "==> [4/5] send a real POST /v1/chat/completions"
REQUEST='{"model":"gpt-4o","messages":[{"role":"user","content":"what database should I use?"}]}'
echo "    client sent: $REQUEST"
curl -fsS "http://$OIKOS_ADDR/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "$REQUEST" > "$WORK/client_resp.json"
echo

echo "==> [5/5] the request body the UPSTREAM actually received:"
echo "------------------------------------------------------------------"
# Extract the echoed body line and pretty-print it.
FORWARDED="$(grep -a '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" | head -1 | sed 's/^FORWARDED_REQUEST_BODY://')"
if command -v python3 >/dev/null 2>&1; then
  echo "$FORWARDED" | python3 -m json.tool
else
  echo "$FORWARDED"
fi
echo "------------------------------------------------------------------"
echo

# Verdict (relevant case).
if echo "$FORWARDED" | grep -q '<!-- oikos:rules:begin v=1 -->' && \
   echo "$FORWARDED" | grep -q 'Always use the PostgreSQL database, never MySQL.'; then
  echo "RESULT: PASS — oikos injected the 'Use Postgres' rule as a leading"
  echo "        system message; the client never sent it. The upstream saw the rule."
else
  echo "RESULT: FAIL — no injected oikos block found in the forwarded request."
  echo "        oikosd.log:"; sed 's/^/          /' "$WORK/oikosd.log"
  exit 1
fi
echo

# ---------------------------------------------------------------------------
# [6/6] F-A proof: an IRRELEVANT user message gets NO injection. The same vault
# (one DB rule), a weather question → the similarity floor rejects it, so the
# upstream sees the ORIGINAL request with ZERO oikos block. This proves the
# similarity floor is reachable (the M2 review F-A fix) — irrelevant rules are
# NOT injected into ~every request.
echo "==> [6/6] send an IRRELEVANT message — expect NO injection (F-A floor proof)"
# Don't truncate the upstream log (the fake upstream holds it open without
# O_APPEND, so a truncate leaves a NUL hole). Instead, count the lines already
# present and read the NEXT (last) forwarded body after this request.
BEFORE_N="$(grep -ac '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" || true)"
IRRELEVANT='{"model":"gpt-4o","messages":[{"role":"user","content":"what is the weather today?"}]}'
echo "    client sent: $IRRELEVANT"
curl -fsS "http://$OIKOS_ADDR/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d "$IRRELEVANT" > "$WORK/client_resp2.json"
echo
# Wait until a NEW forwarded line appears (don't let a pending grep trip set -e).
for _ in $(seq 1 50); do
  NOW_N="$(grep -ac '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" || true)"
  if [[ "${NOW_N:-0}" -gt "${BEFORE_N:-0}" ]]; then break; fi
  sleep 0.05
done
FORWARDED2="$(grep -a '^FORWARDED_REQUEST_BODY:' "$WORK/upstream.log" | tail -1 | sed 's/^FORWARDED_REQUEST_BODY://')"
echo "    the request body the UPSTREAM received:"
echo "------------------------------------------------------------------"
if command -v python3 >/dev/null 2>&1; then
  echo "$FORWARDED2" | python3 -m json.tool
else
  echo "$FORWARDED2"
fi
echo "------------------------------------------------------------------"
echo
if echo "$FORWARDED2" | grep -q '<!-- oikos:rules:begin'; then
  echo "RESULT: FAIL — an irrelevant message got an oikos block injected (floor unreachable)."
  exit 1
elif [[ "$FORWARDED2" == "$IRRELEVANT" ]]; then
  echo "RESULT: PASS — the irrelevant weather question was forwarded VERBATIM with"
  echo "        ZERO injection. The similarity floor rejected the off-topic DB rule (F-A)."
  exit 0
else
  echo "RESULT: FAIL — forwarded body was not the verbatim original:"
  echo "          $FORWARDED2"
  exit 1
fi
