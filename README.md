# oikos

> **oikos watches your AI corrections and auto-writes & maintains your `AGENTS.md`
> (plus `CLAUDE.md` / `GEMINI.md` mirrors) — the preference you teach once stays
> current in every tool's native rules file, with quarantine. White, pure-Go,
> single binary, nothing leaves your machine.**

`AGENTS.md` is now a [Linux-Foundation-stewarded standard](https://agents.md): drop
one file in your repo and Cursor, Claude Code, Codex, Gemini and the rest read it.
That's "one rule → all tools" — for free. **oikos doesn't compete with that. It
rides it.**

The catch with a static `AGENTS.md` is simple: **a file doesn't learn.** You
hand-write it, and it goes stale the moment your preferences change. oikos closes
that loop — it captures the corrections you give your AI, distills them into
editable Markdown rules in your own vault, ranks them, and **keeps your `AGENTS.md`
written and current for you.** You teach a preference once; it stays live in every
tool that reads the standard.

---

## What oikos actually does

```
your AI corrections ──▶ oikos vault (editable .md rules, ranked) ──▶ AGENTS.md
                                                                     CLAUDE.md
                                                                     GEMINI.md
```

1. **Auto-writes your `AGENTS.md`.** `oikos emit` regenerates the ranked rule block
   into your `AGENTS.md` (and `CLAUDE.md` / `GEMINI.md` mirrors) from your vault —
   on demand, **with no proxy running.** Only your own content is touched: oikos
   owns exactly one fenced region (`<!-- oikos:rules:begin … end -->`); everything
   you wrote by hand is preserved byte-for-byte.
2. **Keeps it current.** As you teach corrections, your rules change — re-run
   `oikos emit` (or leave the optional proxy on for continuous updates) and the
   file reflects the latest ranked set. A static file you maintain by hand can't
   do that.
3. **Learns the corrections.** A correction becomes an editable `.md` rule in your
   vault:
   - `T0` — explicit sigil: `/remember Always use PostgreSQL, never MySQL` → a rule, immediately.
   - `T1` — zero-token heuristic: a detected correction → a draft in `_inbox/` (quarantine).
   - `T2` — optional, opt-in, local-preferred LLM distillation (default **off**; never uploads
     your exchanges silently).
   Drafts are promoted only on independent reinforcement. **Credentials are never persisted.**

The leverage is the layer *above* the model — ranked, correction-learned rules that
follow you across every tool that reads `AGENTS.md` — not the model itself, so it
keeps working even as models plateau.

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/deios0/oikos/master/scripts/install.sh | sh
```

Downloads the signed static binary for your platform into a `PATH` dir that needs no
`sudo` (e.g. `~/.local/bin`), verifies its SHA-256, and does nothing else — no
service, no runtime state. Or grab a binary straight from
[Releases](https://github.com/deios0/oikos/releases), or `go install
github.com/deios0/oikos/cmd/oikos@latest`.

### Verify the download (optional, recommended)

Every release ships `SHA256SUMS` plus a minisign signature over it. Verify with the
project's public key (also in [`minisign.pub`](./minisign.pub)):

```bash
minisign -Vm SHA256SUMS -P RWQMmjBv33uhQw/mwvr1/2VzO8+n/hou4gEHT6Yr97pF1Uj3yoiM7mNw
sha256sum -c SHA256SUMS --ignore-missing        # then check your binary against it
```

## Quickstart

```bash
oikos init                                   # seed a vault + a starter rule
oikos emit --file claude-code=./CLAUDE.md \
           --file codex=./AGENTS.md          # write the ranked block into your files
# …or, with tools wired via `oikos wire`, just:
oikos emit                                   # uses the vault + native files from config
```

That's the whole product. No proxy, no service, no account — `oikos emit` reads your
vault and writes your native rules files. Drop more `.md` rules into the vault (or
let oikos learn them), re-run `oikos emit`, and every tool stays current.

---

## Live mode (optional)

If you *want* real-time capture, run the optional local proxy:

```
your AI tool ──▶ oikos · 127.0.0.1:4141 ──▶ inject your rules ──▶ your model
                                                                      │
              learns the correction ◀── verbatim stream back ◀───────┘
```

- **`oikos serve`** is a white, BYOK, single-binary loopback proxy. Point any
  OpenAI-compatible tool's base URL at `http://127.0.0.1:4141` and oikos (a) injects
  your relevant rules per request and (b) captures your corrections **live**, the
  moment you make them — no manual `oikos emit`.
- **Relevant-only, fail-open.** Only rules sharing vocabulary with the request are
  injected; an off-topic request is forwarded **byte-for-byte**, untouched. The
  intercept is in-memory and **fail-open under 15 ms** — it never corrupts or
  delays a request.
- It's strictly opt-in. The default path — `oikos emit` writing `AGENTS.md` — needs
  no proxy, no key, and no model.

Live mode is the same engine, just continuous instead of on-demand. The file it
writes is byte-identical to what `oikos emit` produces.

---

## Why oikos, honestly

|                                  | bare `AGENTS.md` | oikos | Headroom (proxy) |
|----------------------------------|:---:|:---:|:---:|
| One rule → all tools             | ✅ (the standard) | ✅ (rides it) | ✅ |
| **Auto-written from corrections**| ❌ you hand-write it | ✅ | ⚠️ proxy-only |
| **Stays current as you teach**   | ❌ goes stale | ✅ | ✅ (while proxy runs) |
| Works with **no proxy running**  | ✅ | ✅ (`oikos emit`) | ❌ proxy is the product |
| Nothing leaves your machine      | ✅ | ✅ (zero phone-home) | ⚠️ hosted/cloud options |
| Pure Go, **single static binary**| n/a | ✅ (CGO-free) | ❌ |
| Quarantine for unproven rules    | ❌ | ✅ (`_inbox/` drafts, reinforce-to-promote) | varies |
| Credentials never persisted      | n/a | ✅ (lexicon + path refusal) | varies |
| Price                            | free | **free** | free tier + paid |

Full framing: [`docs/positioning.md`](docs/positioning.md).

oikos is **free for everyone** — there is no paid tier, no hosted tier, no
governance/compliance layer to upsell. It is a tool that keeps your own file
current on your own machine.

---

## What it is — and what it isn't

**It is:** a local, single-binary tool that turns your AI corrections into ranked,
editable Markdown rules and keeps your `AGENTS.md` / `CLAUDE.md` / `GEMINI.md`
written from them.

**It isn't:** a model, a hosted service, an account, a telemetry pipe, or a
governance/compliance product. It writes one fenced region in files you own and
phones home to nothing.

---

## Three locked invariants

- **Purity / "white".** Inert until *you* act. No state until first use. **Zero
  phone-home** — `oikos emit` touches only your local files; the only thing the
  optional proxy ever dials is *your* chosen upstream.
- **< 15 ms, fail-open (live mode).** If anything is slow or ambiguous, the request
  passes through verbatim. The proxy never breaks a request.
- **Owns one fenced region, nothing else.** Every emit replaces only the
  `<!-- oikos:rules:begin … end -->` block (backed up on first write, atomic,
  idempotent). Your hand-written content is never touched, and a path containing a
  tracked credential is refused outright.

---

## Writing rules (so they actually fire)

Selection is **relevance-gated**: oikos emits/injects a rule only when it shares
vocabulary with the request (a lexical floor, augmented with curated concept
expansion). So a rule has to contain the words of the requests you want it to catch.

> **Put the vocabulary of the request into the rule.** A "use PostgreSQL" rule
> should also say *"database"* if you want it to fire on a database question — the
> model won't type "PostgreSQL", *you'll* type "database".

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

- **Pure Go, single static binary** (`CGO_ENABLED=0`). No Docker, no Postgres, no
  Qdrant, no venv, no ONNX, no model download — the install simplicity is the point.
- Intelligence is **ported** from a production rule engine (graded rule store with
  ranking / promote-demote / confidence / decay, BM25 + curated concept-expansion
  retrieval, a credential lexicon) — proven, not invented.
- BYOK upstream (live mode only): OpenRouter via key, or an auto-detected local
  Ollama / LM Studio.

```
cmd/oikos/        the oikos CLI — `emit` (standalone, no proxy) · `serve` (live mode)
                  · `init` · `wire`/`unwire` · `sync`
internal/
  emit/           the NativeFileEmitter (CLAUDE.md / AGENTS.md / GEMINI.md)
  rules/          the graded rule index, relevance floor, eager emitter
  extract/        learns-back: T0 sigil · T1 heuristic · credential lexicon
  server/         the optional loopback proxy, middleware, capture tap
  inject/         the byte-exact rule splicer (single-parse, single-copy, fail-open)
  capture/        off-path SSE reassembly (never delays the client stream)
  lifecycle/      dedup · reinforce · decay · supersede · promote
  learn/ upstream/ secret/ auth/
docs/             positioning · vision · specs · architecture · research · review
scripts/          demo-learn.sh · demo-injection.sh
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

oikos is **free and open**. The whole tool — the rule store, injection,
learns-back extraction, the native-file emitter, and the optional proxy (this
repository) — is licensed under the **Apache License 2.0** (see [`LICENSE`](LICENSE)).
There is no separate proprietary tier.

*Author: Denis Alaev.*
