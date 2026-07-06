<div align="center">

# oikos

### Your AI stops repeating mistakes.

**A local memory that learns the corrections you give your AI — and makes them stick
across every tool.** Fix something once in Cursor, and Claude Code, Codex and Gemini
stop making that same mistake too. You bring your own keys; oikos is a single white,
pure-Go binary that's inert until you use it — nothing leaves your machine, ever.

[![release](https://img.shields.io/github/v/release/deios0/oikos?color=0b7cc4&label=release)](https://github.com/deios0/oikos/releases)
[![license](https://img.shields.io/badge/license-Apache--2.0-0b7cc4)](LICENSE)
[![platforms](https://img.shields.io/badge/platforms-macOS%20·%20Linux%20·%20Windows-555)](https://github.com/deios0/oikos/releases)
[![pure Go](https://img.shields.io/badge/pure%20Go-CGO__free%20static%20binary-00ADD8)](#architecture)
[![phone-home](https://img.shields.io/badge/phone--home-none-1a7f43)](#three-locked-invariants)

```bash
curl -fsSL https://raw.githubusercontent.com/deios0/oikos/master/scripts/install.sh | sh
```

</div>

---

Every AI tool forgets. You correct Cursor — *"use PostgreSQL, not MySQL"* — and tomorrow
Codex suggests MySQL again. The fix you already taught doesn't travel: not to the next
tool, not to next week. Your preferences live in your head; the model starts from zero
every session.

**oikos is the memory layer that fixes this.** It captures the corrections you give your
AI, distills each into an editable Markdown rule you own, ranks them, and feeds the
relevant ones back into every tool — so a preference you teach once *stays taught*,
everywhere. The leverage is the layer **above** the model — a correction ledger that's
yours and portable — so it keeps paying off even as models plateau.

It rides the open [`AGENTS.md` standard](https://agents.md) (one file → Cursor, Claude
Code, Codex, Gemini all read it) and adds the one thing a static file can't do: **it
learns.** Optionally, it can also sit live between your tools and the model, catching
corrections the moment you make them. And when you're ready for a team, the same binary
[connects to a shared server](#team-tier--connect-to-a-server-optional) so your whole
team's corrections compound together.

---

## How it works

```
your AI corrections ──▶ oikos vault (editable .md rules, ranked) ──▶ AGENTS.md
                                                                     CLAUDE.md
                                                                     GEMINI.md
```

1. **Learns the corrections.** A correction becomes an editable `.md` rule in your own
   vault — Markdown you can read, edit, and grep:
   - `T0` — explicit: `/remember Always use PostgreSQL, never MySQL` → a rule, immediately.
   - `T1` — zero-token heuristic: a detected correction → a draft in `_inbox/` (quarantine).
   - `T2` — optional, opt-in, local-preferred LLM distillation (default **off**; never uploads
     your exchanges silently).

   Drafts are promoted only on independent reinforcement. **Credentials are never persisted.**
2. **Enforces them across tools.** `oikos emit` writes the ranked, relevant rules into your
   `AGENTS.md` (and `CLAUDE.md` / `GEMINI.md` mirrors) — on demand, **no proxy needed.** Only
   your own content is touched: oikos owns exactly one fenced region
   (`<!-- oikos:rules:begin … end -->`); everything you wrote by hand is preserved byte-for-byte.
3. **Keeps it current.** Teach a new correction, re-run `oikos emit` (or leave the optional
   live proxy on), and every tool reflects the latest ranked set. A static file you maintain
   by hand can't do that.

The leverage is the layer *above* the model — a ranked, correction-learned rule ledger that's
yours and follows you across every tool — not the model itself, so it keeps paying off even as
models plateau.

---

## Install

```bash
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/deios0/oikos/master/scripts/install.sh | sh

# Windows (PowerShell)
irm https://raw.githubusercontent.com/deios0/oikos/master/scripts/install.ps1 | iex
```

Downloads the signed static binary for your platform into a `PATH` dir that needs no `sudo`
(e.g. `~/.local/bin`), verifies its SHA-256, and does nothing else — no service, no runtime
state. Or grab a binary from [Releases](https://github.com/deios0/oikos/releases), or
`go install github.com/deios0/oikos/cmd/oikos@latest`.

<details>
<summary><b>Verify the download</b> (optional, recommended)</summary>

Every release ships `SHA256SUMS` plus a minisign signature over it. Verify with the
project's public key (also in [`minisign.pub`](./minisign.pub)):

```bash
minisign -Vm SHA256SUMS -P RWQMmjBv33uhQw/mwvr1/2VzO8+n/hou4gEHT6Yr97pF1Uj3yoiM7mNw
sha256sum -c SHA256SUMS --ignore-missing        # then check your binary against it
```
</details>

---

## Quickstart

```bash
oikos init                                   # seed a vault + a starter rule
oikos emit --file claude-code=./CLAUDE.md \
           --file codex=./AGENTS.md          # write the ranked block into your files
# …or, with tools wired via `oikos wire`, just:
oikos emit                                   # uses the vault + native files from config
```

That's the whole product. No proxy, no service, no account — `oikos emit` reads your vault
and writes your native rules files. Drop more `.md` rules into the vault (or let oikos learn
them), re-run `oikos emit`, and every tool stays current.

---

## Live mode (optional)

If you *want* real-time capture, run the optional local proxy:

```
your AI tool ──▶ oikos · 127.0.0.1:4141 ──▶ inject your rules ──▶ your model
                                                                      │
              learns the correction ◀── verbatim stream back ◀───────┘
```

- **`oikos serve`** is a white, BYOK, single-binary loopback proxy. Point any
  OpenAI-compatible tool's base URL at `http://127.0.0.1:4141` and oikos (a) injects your
  relevant rules per request and (b) captures your corrections **live**, the moment you make
  them — no manual `oikos emit`.
- **Relevant-only, fail-open.** Only rules sharing vocabulary with the request are injected;
  an off-topic request is forwarded **byte-for-byte**, untouched. The intercept is in-memory
  and **fail-open under 15 ms** — it never corrupts or delays a request.
- It's strictly opt-in. The default path — `oikos emit` writing `AGENTS.md` — needs no proxy,
  no key, and no model.

Live mode is the same engine, just continuous instead of on-demand. The file it writes is
byte-identical to what `oikos emit` produces.

---

## Team tier — connect to a server (optional)

Everything above is **free, local, and needs no account.** But a solo vault is only half the
idea. Point oikos at an **oikos server** and it becomes a team's shared brain:

```bash
# join your team's shared rule store + coordination bus, in one command
oikos onboard \
  --endpoint      https://bus.your-team.example/aibus/events \
  --key-file      ~/.config/oikos/keys/bus.key \
  --brain-endpoint https://brain.your-team.example \
  --brain-key-file ~/.config/oikos/keys/brain.key \
  --file claude-code=./CLAUDE.md
```

- **Shared rules (Brain).** Your team's rules — learned and ranked centrally — are pulled
  into every member's `AGENTS.md`, scoped to your zone. The server decides which rules by
  your key; a member never asserts their own access.
- **Coordination (aibus).** A realtime event bus so tools, agents, and teammates coordinate
  across machines.
- **Zones.** Onboarding a person becomes *"install the binary + one command."* Access is
  server-enforced per zone — walled collaboration for a team, a client, or the public.

**Two ways to get a server:**

- **Build your own.** The client speaks a small, open protocol — see the `internal/bus`
  and `internal/brain` clients in this repo (plain HTTP: a zone key in a header, a rule
  store behind `GET /api/rules`, an event bus behind `GET/POST …/events`). The endpoints in
  the example above are placeholders — point them at a server you stand up, and the same
  free binary is now your team's shared brain.
- **Or come to us.** We run a managed Brain + bus + model-routing tier so you don't have to
  operate one. *(Hosted tier is rolling out — no public URL yet; get in touch.)*

The **local client stays free and open** (this repo, Apache-2.0). The **server** — the
Brain + bus + model-routing tier that turns oikos into team infrastructure — is the
paid / self-managed layer. Same white binary for everyone; capability comes from the key you
add. *(Model routing / Bridge integrates as a separate MCP, and can be used on any project.)*

> This is the open-core line: the tool is free forever; the **team server** is where oikos
> becomes an "AI OS" for an organization — bring your own, or let us host it.

---

## Why oikos, honestly

|                                  | bare `AGENTS.md` | oikos | proxy-only tools |
|----------------------------------|:---:|:---:|:---:|
| One rule → all tools             | ✅ (the standard) | ✅ (rides it) | ✅ |
| **Auto-written from corrections**| ❌ you hand-write it | ✅ | ⚠️ proxy-only |
| **Stays current as you teach**   | ❌ goes stale | ✅ | ✅ (while proxy runs) |
| Works with **no proxy running**  | ✅ | ✅ (`oikos emit`) | ❌ proxy is the product |
| Nothing leaves your machine      | ✅ | ✅ (zero phone-home) | ⚠️ hosted/cloud options |
| Pure Go, **single static binary**| n/a | ✅ (CGO-free) | ❌ |
| Quarantine for unproven rules    | ❌ | ✅ (`_inbox/` drafts, reinforce-to-promote) | varies |
| Credentials never persisted      | n/a | ✅ (lexicon + path refusal) | varies |
| Team sync / shared rules         | ❌ | ✅ (optional server tier) | some (hosted) |

Full framing: [`docs/positioning.md`](docs/positioning.md).

---

## What it is — and what it isn't

**It is:** a local, single-binary tool that turns your AI corrections into ranked, editable
Markdown rules and keeps your `AGENTS.md` / `CLAUDE.md` / `GEMINI.md` written from them —
plus an optional client to a shared team server.

**It isn't:** a model, a telemetry pipe, or something that phones home. The local tool writes
one fenced region in files you own and dials nothing on its own; the only outbound the proxy
ever makes is to *your* chosen upstream, and the only outbound the client makes is to the
server *you* explicitly join.

---

## Three locked invariants

- **Purity / "white".** Inert until *you* act. No state until first use, no socket until you
  `join`/`serve`. **Zero phone-home** — `oikos emit` touches only your local files.
- **< 15 ms, fail-open (live mode).** If anything is slow or ambiguous, the request passes
  through verbatim. The proxy never breaks a request.
- **Owns one fenced region, nothing else.** Every emit replaces only the
  `<!-- oikos:rules:begin … end -->` block (backed up on first write, atomic, idempotent).
  Your hand-written content is never touched, and a path containing a tracked credential is
  refused outright.

---

## Writing rules (so they actually fire)

Selection is **relevance-gated**: oikos emits/injects a rule only when it shares vocabulary
with the request (a lexical floor, augmented with curated concept expansion). So a rule has
to contain the words of the requests you want it to catch.

> **Put the vocabulary of the request into the rule.** A "use PostgreSQL" rule should also
> say *"database"* if you want it to fire on a database question — the model won't type
> "PostgreSQL", *you'll* type "database".

```markdown
❌  Use PostgreSQL.
     → "which database should I use?" shares no word → not selected.

✅  For any database / data store / persistence choice, default to PostgreSQL,
    never MySQL. Prefer Postgres over SQLite for anything multi-process.
     → "database", "data store", "persistence" are in the rule → it fires.
```

Full guide: [`docs/writing-rules.md`](docs/writing-rules.md).

---

## Architecture

- **Pure Go, single static binary** (`CGO_ENABLED=0`). No Docker, no Postgres, no Qdrant, no
  venv, no ONNX, no model download — the install simplicity is the point.
- Intelligence is **ported** from a production rule engine (graded rule store with ranking /
  promote-demote / confidence / decay, BM25 + curated concept-expansion retrieval, a
  credential lexicon) — proven, not invented.
- BYOK upstream (live mode only): OpenRouter via key, or an auto-detected local Ollama / LM Studio.

```
cmd/oikos/        the CLI — emit · serve · init · wire/unwire · sync · join/leave/bus/brain · onboard
internal/
  emit/           the NativeFileEmitter (CLAUDE.md / AGENTS.md / GEMINI.md)
  rules/          the graded rule index, relevance floor, eager emitter
  extract/        learns-back: T0 sigil · T1 heuristic · credential lexicon
  server/         the optional loopback proxy, middleware, capture tap
  inject/         the byte-exact rule splicer (single-parse, single-copy, fail-open)
  capture/        off-path SSE reassembly (never delays the client stream)
  lifecycle/      dedup · reinforce · decay · supersede · promote
  bus/ brain/     the opt-in clients for a team server (bus + shared rule store)
  learn/ upstream/ secret/ auth/ wire/
scripts/          demo-learn.sh · demo-injection.sh · install.sh · install.ps1
```

---

## Demos

```bash
go build ./...
bash scripts/demo-learn.sh          # a correction → a rule → emitted to AGENTS.md (no proxy)
bash scripts/demo-injection.sh      # live mode: a rule injected + an off-topic query passed verbatim
```

---

## Licensing

The **oikos tool** in this repository — the rule store, injection, learns-back extraction,
the native-file emitter, the optional proxy, and the opt-in team-server clients — is licensed
under the **Apache License 2.0** (see [`LICENSE`](LICENSE)) and is **free forever**. The
optional **team server** (hosted Brain + bus + model routing) is a separate, managed layer.

*Author: Denis Alaev.*
