package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"oikos/internal/wire"
)

// runWire implements `oikos wire <tool>`. It resolves the correct channel for
// the tool, applies the wiring (persisting it + seeding a native-file block when
// needed), and prints a clear next step. It is idempotent.
//
//	oikos wire cursor          # base_url tool → prints the env-export to set
//	oikos wire claude-code     # native-file tool → seeds the managed block
//	oikos wire --dir <path> X  # anchor a native-file tool's file under <path>
func runWire(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("wire", flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("dir", "", "directory to anchor a native-file tool's instruction file (default: cwd)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: oikos wire [--dir <path>] <tool>\n" +
			"  tools: cursor, claude-code, continue, codex, or any OpenAI-compatible tool name")
	}
	tool := fs.Arg(0)

	plan, err := wire.Resolve(tool, *dir)
	if err != nil {
		return err
	}
	if _, err := wire.Apply(plan); err != nil {
		return err
	}
	fmt.Fprintf(out, "oikos: %s\n", plan.Summary())
	return nil
}

// runUnwire implements `oikos unwire <tool>`. It cleanly undoes a prior wire:
// restores a native-file tool's instruction file byte-exact from the .oikos.bak
// backup (or strips the managed block when oikos created the file), and removes
// the persisted wiring record. It is idempotent — unwiring a tool that is not
// wired is a clean no-op.
//
//	oikos unwire claude-code      # restore CLAUDE.md, drop the record
//	oikos unwire --dir <path> X   # anchor the native-file tool under <path>
func runUnwire(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("unwire", flag.ContinueOnError)
	fs.SetOutput(out)
	dir := fs.String("dir", "", "directory to anchor a native-file tool's instruction file (default: cwd)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: oikos unwire [--dir <path>] <tool>\n" +
			"  tools: cursor, claude-code, continue, codex, or any wired tool name")
	}
	tool := fs.Arg(0)

	outcome, err := wire.UnwireResult(tool, *dir)
	if err != nil {
		return err
	}

	// Print an HONEST message (P1-BUG-2): don't claim "original config restored" for
	// a base_url tool whose config oikos couldn't touch. Native-file tools are
	// restored byte-exact from the backup; base_url tools depend on whether oikos
	// knew where the tool's IDE config lives and had written its proxy URL there.
	switch outcome.Channel {
	case wire.ChannelBaseURL:
		switch {
		case outcome.BaseURL.Changed:
			fmt.Fprintf(out, "oikos: unwired %s — removed the oikos proxy URL from %s (restored the vendor default) and dropped the wiring record.\n",
				tool, outcome.BaseURL.Path)
		case outcome.BaseURL.NeedsManualHint:
			fmt.Fprintf(out, "oikos: unwired %s — wiring record removed.\n"+
				"  oikos couldn't locate this tool's config to auto-restore it. If you (or oikos's self-heal) pointed the tool's API base URL at %s,\n"+
				"  change it back to the vendor default (e.g. https://api.openai.com/v1) MANUALLY, or the tool will keep hitting a now-dead proxy.\n",
				tool, wire.ProxyBaseURL+"/v1")
		default:
			fmt.Fprintf(out, "oikos: unwired %s — wiring record removed (its config did not point at the oikos proxy, so nothing to restore).\n", tool)
		}
	default:
		fmt.Fprintf(out, "oikos: unwired %s — original config restored, wiring record removed.\n", tool)
	}
	return nil
}

// readFileBytes is a tiny indirection so the wire test can read a file without
// importing os twice; it also keeps the test package self-contained.
func readFileBytes(path string) ([]byte, error) { return os.ReadFile(path) }
