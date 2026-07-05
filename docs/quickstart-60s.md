# oikos in 60 seconds

Three commands. No account, no key, no model, nothing leaves your machine.

```bash
oikos init                            # seed a vault + one starter rule
oikos emit --file codex=./AGENTS.md   # write the ranked rule block into AGENTS.md
#                                       (add --file claude-code=./CLAUDE.md for a mirror)
```

Open `AGENTS.md`: the fenced `<!-- oikos:rules:begin … end -->` block now holds
your ranked rules. Everything you wrote by hand outside that block is untouched,
byte-for-byte. That is the whole product — a vault of editable `.md` rules that
oikos keeps written into every tool's native rules file.

Teach it a preference and re-emit:

```bash
printf '%s\n' '---' 'id: postgres' 'title: Use PostgreSQL' 'status: live' 'weight: 0.9' '---' \
  'Always use PostgreSQL, never MySQL.' > ~/oikos-vault/postgres.md   # default vault; override with OIKOS_VAULT
oikos emit                                 # AGENTS.md now reflects it — or /remember … through the proxy
```

## It works with nothing configured (graceful degrade)

`oikos emit` needs **no model, no API key, no network** — it only reads your
vault and writes your files. If you have not set an upstream model or an
OpenRouter/OpenAI key, emit still works fully; oikos just prints a one-line
reminder that *live* capture (the optional proxy) is unavailable until you add a
key. A missing key degrades a feature — it never breaks the tool.

## Verify it doesn't phone home (30 seconds)

oikos is white by construction, and you can check that yourself before trusting it:

```bash
# 1. The binary links nothing and calls out to nothing on its own.
oikos emit                      # produces your file with the network OFF — try it airplane-mode.

# 2. The only outbound connection oikos EVER makes is the proxy forwarding YOUR
#    request to YOUR configured upstream — and only while `oikos serve` runs.
#    With no proxy running, there is no socket. Confirm on your box:
oikos serve &                   # optional live mode
ss -tanp | grep oikos           # you will see ONLY a listener on 127.0.0.1:4141
```

There is no telemetry, no update check, no "call home". `oikos emit` — the
default path — opens no socket at all. When `oikos serve` is running it forwards
to the upstream **you** set (`oikos wire` / config), stripping the loopback token
first, and forwards off-topic requests byte-for-byte.

## Then, optionally, live mode

```bash
oikos serve                     # loopback proxy on 127.0.0.1:4141
# point any OpenAI-compatible tool's base URL at http://127.0.0.1:4141
```

Now corrections are captured the moment you make them — no manual `oikos emit`.
Same engine, continuous instead of on-demand. Fail-open under 15 ms; it never
delays or corrupts a request.
