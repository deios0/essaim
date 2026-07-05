package rules

import (
	"os"
	"path/filepath"
)

// InboxDir is the vault subdirectory that holds the LOCAL, ephemeral-hot working
// area: status:draft rules awaiting review and the .counts.json reinforce-counter
// sidecar. Everything under it is per-machine and MUST NOT be committed — drafts
// are unreviewed and the sidecar is hot, machine-local state (the 3-class
// discipline / frontmatter-immutability rule). One definition; both the extractor (drafts)
// and the lifecycle hot store (sidecar) reference it.
const InboxDir = "_inbox"

// inboxGitignore is the content written to _inbox/.gitignore on first use. It
// ignores EVERYTHING under _inbox/ except the .gitignore itself, so a user whose
// vault is a git repo never accidentally commits .counts.json or an unreviewed
// draft — current or future (a new sidecar file is covered automatically). The
// `!.gitignore` un-ignore keeps the marker itself trackable (so the protection is
// visible/portable), without listing each file pattern by name.
const inboxGitignore = "# oikos: local, ephemeral-hot working area — never commit.\n" +
	"# Holds unreviewed draft rules and the .counts.json reinforce-counter sidecar.\n" +
	"*\n" +
	"!.gitignore\n"

// EnsureInboxDir creates the vault's _inbox/ directory (idempotent) and, on FIRST
// use, drops an _inbox/.gitignore so a git-tracked vault never accidentally
// commits the local drafts/sidecar. It returns the absolute-ish inbox dir path
// (filepath.Join(vault, InboxDir)). The .gitignore is written only when ABSENT, so
// an operator-customized .gitignore is never clobbered on subsequent calls. A
// .gitignore write failure is best-effort and does NOT fail the call — the inbox
// must still be usable even on a read-only-ish FS where only the marker can't be
// written; the only hard error is failing to create the directory itself.
func EnsureInboxDir(vault string) (string, error) {
	dir := filepath.Join(vault, InboxDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, err
	}
	giPath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(giPath); os.IsNotExist(err) {
		// Best-effort: a marker-write failure must not break inbox usage.
		_ = os.WriteFile(giPath, []byte(inboxGitignore), 0o644)
	}
	return dir, nil
}
