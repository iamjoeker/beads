package main

import (
	"strings"

	"github.com/spf13/cobra"
)

// resolveJSONOutput decides the output mode for the command that is about to
// run, from that command's own flags and — only when the command asked for
// nothing — from configJSON (the `json` config key).
//
// This is the one place production code decides the output mode, and callers
// must assign its result unconditionally. jsonOutput is a package global, so
// a command that sets it from a flag inside its own Run leaves the value
// standing for whatever runs next. The real CLI never notices (the process
// exits after one command); an in-process test binary always does, and the
// next test asserting on human-readable output gets JSON instead. Resolving
// once per command, with every branch below returning a value, makes the
// previous command's setting unobservable.
//
// It reads through cmd.Flags(), which at execution time carries the inherited
// persistent flags as well as the command's own. That distinction is load
// bearing: compact, migrate, repo and restore each declare a LOCAL --json,
// and pflag keeps the first flag registered under a name, so the root's
// persistent --json never reaches their flag set. Resolving from
// cmd.Root().PersistentFlags() alone therefore sees Changed("json") == false
// for `bd repo list --json` and falls through to config — which is why that
// command printed human output while `bd list --json` printed JSON.
func resolveJSONOutput(cmd *cobra.Command, configJSON bool) bool {
	if cmd == nil {
		return configJSON
	}
	flags := cmd.Flags()

	formatChanged := flags.Changed("format")
	jsonChanged := flags.Changed("json")

	// --format json is the alias for --json (desire-path from GH#2612). It is
	// checked first so it wins over an unset --json, and it is checked on the
	// command's own flag set so list's and dep tree's local --format (which
	// also spells 'digraph', 'dot', 'mermaid' and Go templates) route their
	// json value here instead of setting the global from inside their Run.
	if formatChanged {
		if format, err := flags.GetString("format"); err == nil && strings.EqualFold(format, "json") {
			return true
		}
	}

	if jsonChanged {
		// An explicit --json=false asks for human output and must beat a
		// `json: true` in config, so the flag's value is returned as given
		// rather than OR-ed with anything.
		if v, err := flags.GetBool("json"); err == nil {
			return v
		}
		return false
	}

	if formatChanged {
		// --format named something other than json, and --json was not given.
		// The command asked for a non-JSON rendering, so config must not
		// override it back to JSON.
		return false
	}

	return configJSON
}
