package rules

import "strings"

// concepts.go — M5 SEMANTIC relevance via a curated, embedded concept-expansion
// table. It closes the lexical floor's "needs a shared word" gap.
//
// ⚠️ CORRECTION (m5-semantic-fix): the original comment here claimed this table is
// "ZERO false positives BY CONSTRUCTION." THAT WAS FALSE — proven by a 45-query
// off-topic battery (11 false injects). The table adds concept VALUES ("language",
// "ui", "style", "code", "container", "web", "data", …) to the corpus VOCABULARY,
// and those are common polysemous English words. The no-false-positive property
// does NOT come from "the table can't add an everyday word" (it can); it comes
// from the RELEVANCE FLOOR weighing a query's uncovered content honestly — see
// index.go `Match` (the OOV-content-word denominator) and the ADR's "Correction"
// section. Keep curation tight anyway (below), but never rely on the table alone.
//
// WHY THIS DESIGN (see docs/decisions/2026-06-27-m5-semantic-concept-expansion.md
// for the full ADR + the rejected alternatives):
//
//   - A full transformer (sentence-transformers / ONNX) would force CGO and an
//     external model file — breaking essaim's locked CGO_ENABLED=0 single-static-
//     binary + zero-phone-home invariants. REJECTED.
//   - A dense word-embedding mean-pool (GloVe subset, cosine of mean-pooled
//     vectors) is a few MB and pure-Go, but mean-pool similarity makes
//     EVERYTHING somewhat similar — cos(weather, database) is rarely near zero —
//     so it needs a fragile hand-tuned cosine gate to keep weather at ZERO. It
//     trades a provable invariant for a probabilistic one. REJECTED.
//   - A curated concept-expansion table is a small Go map (compiled INTO the
//     binary — no runtime download, no go:embed asset, no CGO), an O(1) lookup,
//     and it AUGMENTS the existing BM25/floor: a semantic match BECOMES a lexical
//     match on the expanded token set, so it inherits the floor machinery. It does
//     NOT, by itself, guarantee no false positives — the floor does (see above).
//     CHOSEN for purity + simplicity, WITH the corrected floor.
//
// HOW IT WIRES IN (precompute at index time — the <15ms constraint):
//
//   At BuildIndex, each rule's indexed token stream is augmented with the
//   CONCEPT terms of its words (e.g. a rule saying "postgresql" / "mysql" also
//   indexes "database", "sql", "rdbms"). The query path is UNCHANGED — so a
//   query word like "database" is now a real corpus word that the pg rule
//   covers, lifting its absolute relevance `rel` above the floor. The request
//   path adds ZERO per-rule work (all expansion happened at build); the only
//   added per-request cost is that the augmented token bag is slightly larger,
//   which the latency test bounds well under budget.
//
// CURATION DISCIPLINE (limits the BLAST RADIUS — the floor, not this list, is the
// no-false-positive guarantee; keep the list tight so the floor has less to undo):
//
//   - Entries map a SPECIFIC term to its GENERAL concept(s), never the reverse-
//     broad way. "postgresql" → "database" is safe (postgres IS a database);
//     mapping "database" → every DB product would over-fire and is avoided.
//   - Concept terms are themselves topical/distinctive ("database", "sql",
//     "authentication") — never function words or imperatives (those are already
//     excluded from `rel` by relStopwords, so they could never lift a rule over
//     the floor even if added). NOTE many values ARE common English words
//     ("language", "style", "code", "web", "container") — that is unavoidable and
//     is precisely why the floor must count uncovered query content (index.go).
//   - The table is intentionally SMALL and dev/tech-scoped. It is an ontology of
//     terms that appear in engineering RULES, not a general thesaurus. Unrelated
//     everyday vocabulary ("weather", "egg", "mom") has no entry by design.

// conceptExpansions maps a lowercase tech term to the lowercase concept terms it
// implies. The expansion is applied to a RULE's tokens at index-build time:
// indexing a rule that mentions the key ALSO indexes the values, so a query
// using the general concept covers the specific rule. Single source of truth —
// the same table would expand a query if we ever choose query-side expansion;
// today it is rule-side only (all precompute at index time).
//
// Determinism: a Go map literal; iteration order is never observed (we only ever
// LOOK UP by key and append the fixed value slice in slice order). Pure, ASCII,
// lowercase, locale-independent.
var conceptExpansions = map[string][]string{
	// --- Databases & storage -------------------------------------------------
	"postgres":      {"database", "sql", "rdbms"},
	"postgresql":    {"database", "sql", "rdbms"},
	"psql":          {"database", "sql", "rdbms"},
	"mysql":         {"database", "sql", "rdbms"},
	"mariadb":       {"database", "sql", "rdbms"},
	"sqlite":        {"database", "sql"},
	"oracle":        {"database", "sql", "rdbms"},
	"mssql":         {"database", "sql", "rdbms"},
	"cockroachdb":   {"database", "sql", "rdbms"},
	"mongodb":       {"database", "nosql"},
	"mongo":         {"database", "nosql"},
	"cassandra":     {"database", "nosql"},
	"dynamodb":      {"database", "nosql"},
	"redis":         {"database", "cache", "nosql"},
	"memcached":     {"cache"},
	"elasticsearch": {"database", "search"},
	"clickhouse":    {"database", "sql"},
	"duckdb":        {"database", "sql"},
	"rdbms":         {"database", "sql"},
	// Common DB SYNONYMS devs type (specific phrasing → the same DB concept). These
	// are distinctive, unambiguously-storage words, so they enrich a rule's bag
	// without the polysemy risk of generic English words. ("sql"/"nosql" are
	// themselves concept VALUES already produced by the product entries above;
	// listing them as KEYS too lets a rule whose body literally says "sql"/"nosql"
	// also index the broader "database" concept.)
	"datastore":  {"database"},
	"relational": {"database", "sql", "rdbms"},
	"nosql":      {"database"},
	"sql":        {"database"},
	"orm":        {"database", "sql"},

	// --- Languages -----------------------------------------------------------
	"python":     {"language", "code"},
	"py":         {"python", "language", "code"},
	"golang":     {"go", "language", "code"},
	"rust":       {"language", "code"},
	"java":       {"language", "code"},
	"javascript": {"language", "code", "js"},
	"js":         {"javascript", "language", "code"},
	"typescript": {"language", "code", "ts"},
	"ts":         {"typescript", "language", "code"},
	"ruby":       {"language", "code"},
	"php":        {"language", "code"},
	"kotlin":     {"language", "code"},
	"swift":      {"language", "code"},
	"scala":      {"language", "code"},
	"csharp":     {"language", "code"},
	"cpp":        {"language", "code"},
	"node":       {"javascript", "language", "code"},
	"nodejs":     {"javascript", "language", "code"},
	"dotnet":     {"csharp", "language", "code"},

	// --- Web / app frameworks (specific framework → web/api/frontend concepts) -
	// A rule mentioning a framework should fire for a query about the general web
	// or frontend concept. Framework names are distinctive (low polysemy).
	// NOTE: only LOW-polysemy framework names are keys. Ambiguous everyday words
	// ("next", "spring", "gin", "express") are deliberately EXCLUDED — they appear
	// in rule bodies as English far more often than as the framework, and keying
	// them would let an off-topic query hitting "web"/"framework" false-match.
	"django":  {"web", "framework", "api"},
	"flask":   {"web", "framework", "api"},
	"fastapi": {"web", "framework", "api"},
	"rails":   {"web", "framework", "api"},
	"nextjs":  {"frontend", "framework", "web"},
	"nuxt":    {"frontend", "framework", "web"},

	// --- Package / dependency managers (specific tool → dependency concept) ----
	"npm":    {"dependency", "package"},
	"yarn":   {"dependency", "package"},
	"pnpm":   {"dependency", "package"},
	"pip":    {"dependency", "package"},
	"poetry": {"dependency", "package"},
	"cargo":  {"dependency", "package"},
	"gomod":  {"dependency", "package"},
	"maven":  {"dependency", "package", "build"},
	"gradle": {"dependency", "package", "build"},

	// --- Formatting / style tooling -----------------------------------------
	"black":    {"formatter", "format", "style"},
	"autopep8": {"formatter", "format", "style"},
	"prettier": {"formatter", "format", "style"},
	"gofmt":    {"formatter", "format", "style"},
	"rustfmt":  {"formatter", "format", "style"},
	"eslint":   {"linter", "lint", "style"},
	"flake8":   {"linter", "lint", "style"},
	"pylint":   {"linter", "lint", "style"},
	"ruff":     {"linter", "lint", "format", "style"},
	"clippy":   {"linter", "lint", "style"},
	"tabs":     {"indentation", "whitespace", "style"},
	"spaces":   {"indentation", "whitespace", "style"},

	// --- Testing -------------------------------------------------------------
	"pytest":     {"test", "testing"},
	"unittest":   {"test", "testing"},
	"jest":       {"test", "testing"},
	"mocha":      {"test", "testing"},
	"junit":      {"test", "testing"},
	"tdd":        {"test", "testing"},
	"e2e":        {"test", "testing"},
	"regression": {"test", "testing"},
	"rspec":      {"test", "testing"},
	"vitest":     {"test", "testing"},
	"playwright": {"test", "testing", "e2e"},
	"cypress":    {"test", "testing", "e2e"},
	"fixture":    {"test", "testing"},
	"mocking":    {"test", "testing"},

	// --- Containers / infra / deploy ----------------------------------------
	"docker":     {"container", "image", "deploy"},
	"podman":     {"container", "image", "deploy"},
	"kubernetes": {"container", "orchestration", "deploy", "k8s"},
	"k8s":        {"kubernetes", "container", "orchestration", "deploy"},
	"helm":       {"kubernetes", "deploy"},
	"terraform":  {"infrastructure", "infra", "deploy"},
	"ansible":    {"infrastructure", "infra", "deploy"},
	"systemd":    {"service", "daemon"},
	"nginx":      {"server", "proxy", "web"},
	"apache":     {"server", "web"},
	"haproxy":    {"proxy", "loadbalancer"},

	// --- VCS / CI ------------------------------------------------------------
	"git":       {"vcs", "version", "commit"},
	"github":    {"git", "vcs", "ci"},
	"gitlab":    {"git", "vcs", "ci"},
	"jenkins":   {"ci", "pipeline", "build"},
	"circleci":  {"ci", "pipeline", "build"},
	"rebase":    {"git", "vcs", "version"},
	"merge":     {"git", "vcs", "version"},
	"branch":    {"git", "vcs", "version"},
	"pr":        {"git", "vcs", "review"},
	"changelog": {"git", "release", "version"},

	// --- API / web -----------------------------------------------------------
	"rest":      {"api", "http", "endpoint"},
	"graphql":   {"api", "query"},
	"grpc":      {"api", "rpc"},
	"openapi":   {"api", "spec"},
	"swagger":   {"api", "spec"},
	"http":      {"web", "api"},
	"https":     {"web", "api", "tls"},
	"websocket": {"web", "api"},

	// --- Security / auth -----------------------------------------------------
	"oauth":       {"authentication", "auth", "security"},
	"oauth2":      {"authentication", "auth", "security"},
	"jwt":         {"authentication", "auth", "token", "security"},
	"tls":         {"encryption", "security", "https"},
	"ssl":         {"encryption", "security", "tls"},
	"bcrypt":      {"hashing", "password", "security"},
	"argon2":      {"hashing", "password", "security"},
	"credentials": {"secret", "security"},
	"token":       {"secret", "auth"},
	"apikey":      {"secret", "credentials", "auth"},

	// --- Data / ML -----------------------------------------------------------
	"pandas":     {"dataframe", "data"},
	"numpy":      {"array", "data"},
	"polars":     {"dataframe", "data"},
	"tensorflow": {"ml", "machinelearning", "model"},
	"pytorch":    {"ml", "machinelearning", "model"},
	"sklearn":    {"ml", "machinelearning", "model"},

	// --- Frontend ------------------------------------------------------------
	"react":    {"frontend", "ui", "component"},
	"vue":      {"frontend", "ui", "component"},
	"angular":  {"frontend", "ui", "component"},
	"svelte":   {"frontend", "ui", "component"},
	"css":      {"frontend", "style", "ui"},
	"tailwind": {"css", "frontend", "style"},

	// --- Messaging / queues (specific broker → messaging concept) ------------
	"kafka":    {"messaging", "queue", "stream"},
	"rabbitmq": {"messaging", "queue"},
	"sqs":      {"messaging", "queue"},
	"nats":     {"messaging", "queue"},
	"celery":   {"queue", "task", "async"},

	// --- Observability / monitoring (specific tool → observability concept) --
	"prometheus":    {"monitoring", "metrics", "observability"},
	"grafana":       {"monitoring", "metrics", "observability", "dashboard"},
	"datadog":       {"monitoring", "metrics", "observability"},
	"opentelemetry": {"tracing", "observability", "metrics"},
	"sentry":        {"monitoring", "errors", "observability"},

	// --- Cloud providers (specific provider → cloud concept) -----------------
	"aws":            {"cloud", "infrastructure", "infra"},
	"gcp":            {"cloud", "infrastructure", "infra"},
	"azure":          {"cloud", "infrastructure", "infra"},
	"lambda":         {"cloud", "serverless", "function"},
	"s3":             {"cloud", "storage", "object"},
	"cloudformation": {"cloud", "infrastructure", "infra"},

	// --- Concurrency (specific construct → concurrency concept) --------------
	"goroutine": {"concurrency", "parallel"},
	"async":     {"concurrency", "parallel"},
	"await":     {"concurrency", "async"},
	"mutex":     {"concurrency", "lock", "synchronization"},
	"asyncio":   {"concurrency", "async", "parallel"},
}

// expandConcepts returns the curated concept terms implied by a single lowercase
// word, or nil if the word has no entry. Pure, O(1), allocation-free on a miss.
// It is intentionally a one-hop lookup (no transitive chasing): each entry's
// value list already names the GENERAL concepts directly, so a single hop is
// sufficient and avoids any risk of a multi-hop expansion drifting off-topic.
func expandConcepts(word string) []string {
	return conceptExpansions[word]
}

// conceptAugment takes the base word tokens of a rule (already lowercased, no
// trigrams) and returns the DISTINCT concept terms to ADD to that rule's indexed
// token bag. It is called only at BuildIndex (index time) — never on the request
// hot path. The returned terms are plain words (no "_" trigram prefix) so they
// participate in the absolute-relevance `rel` coverage exactly like a body word
// the rule literally contained.
//
// Dedup is against BOTH the words the rule already has AND concepts already
// added, so a rule mentioning both "postgresql" and "mysql" indexes "database"
// once (not twice) — keeping docLen honest for BM25 length-normalization.
func conceptAugment(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	have := make(map[string]bool, len(words))
	for _, w := range words {
		have[w] = true
	}
	var added []string
	for _, w := range words {
		for _, c := range expandConcepts(w) {
			if have[c] {
				continue
			}
			have[c] = true
			added = append(added, c)
		}
	}
	return added
}

// baseWords splits s into its lowercase word tokens WITHOUT trigram augmentation
// — the input to conceptAugment. It mirrors tokenize's word-splitting exactly
// (same isWordRune predicate, same lowercasing) so the concept lookup sees the
// identical tokens BM25 indexes, just without the "_"-prefixed trigrams. Pure,
// deterministic, locale-independent.
func baseWords(s string) []string {
	s = strings.ToLower(s)
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, ch := range s {
		if isWordRune(ch) {
			cur.WriteRune(ch)
		} else {
			flush()
		}
	}
	flush()
	return words
}
