# Writing rules that actually fire

A rule only helps if oikos injects it into the right request. Right now (M4)
matching is **lexical**: oikos injects a rule when the request shares vocabulary
with the rule's text. If your rule and the request have no words in common, the
rule stays out of the way — which is the point (an off-topic request goes through
byte-for-byte), but it also means a rule worded too narrowly can miss requests it
*should* catch.

So the one trick worth knowing:

> **Put the words of the requests you want it to fire on into the rule itself.**

That's it. If you want a "use PostgreSQL" rule to fire when you ask the model a
**database** question, the rule has to actually say "database" — because that's
the word your question will use. The model picking a database won't necessarily
type "PostgreSQL" in its prompt; *you* will type "database".

## Good vs. bad

❌ **Too narrow — won't fire on a database question:**

```markdown
Use PostgreSQL.
```

A request like *"which database should I pick for this app?"* shares **no word**
with that rule ("PostgreSQL" ≠ "database"), so it won't be injected.

✅ **Carries the request's vocabulary — fires:**

```markdown
For any database / data store / persistence choice, default to PostgreSQL —
never MySQL. Use Postgres for relational data and prefer it over SQLite for
anything that outlives a single process.
```

Now the words **database**, **data store**, **persistence**, **relational**,
**SQLite** are all in the rule, so a database question lands on it.

## Rules of thumb

- **Name the topic, not just the answer.** A rule about React file naming should
  say "React", "component", "file" — not only "kebab-case".
- **Include the synonyms people actually type.** "database / data store / DB",
  "test / spec", "deploy / release / ship". One of them will match.
- **A sentence or two beats a fragment.** More on-topic words = more requests it
  catches. (oikos still injects only the *relevant* rules, ranked, within a byte
  budget — being a bit verbose in a rule body doesn't bloat your prompts.)
- **Filler doesn't count.** Common words ("always", "use", "the", "should") and
  pure imperatives carry no topical signal — they're ignored when deciding
  relevance, so don't rely on them to make a rule match.

## Why it works this way

oikos scores each rule against your request by how many of the request's
*distinctive* words the rule's text covers, and injects only the ones over a
relevance floor. That keeps an unrelated rule ("use Postgres") out of an
unrelated request ("what's the weather") instead of stuffing every rule into
every prompt. The cost is the rule has to share the request's vocabulary to be
picked — hence the advice above.

This is a known sharp edge of lexical matching, and it's temporary: **M5** adds a
local semantic embedder, after which "database" will match a "PostgreSQL" rule on
*meaning* even with no shared word. Until then, write the vocabulary in.
