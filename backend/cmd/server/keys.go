package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"regressionmap/internal/apikeys"
)

const keysUsage = `usage:
  server keys create --name NAME --scope admin|analyze
  server keys list
  server keys revoke ID
DATA_DIR must be set; keys live in DATA_DIR/keys.json.`

// runKeys is the "keys" subcommand. It returns the process exit code:
// 0 on success, 1 on a runtime error, 2 on a usage error. "create" prints
// only the secret on stdout so it can be captured by a script.
func runKeys(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	usage := func(msg string) int {
		if msg != "" {
			fmt.Fprintln(stderr, "regressionmap keys:", msg)
		}
		fmt.Fprintln(stderr, keysUsage)
		return 2
	}
	if len(args) == 0 {
		return usage("missing subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create", "list", "revoke":
	default:
		return usage(fmt.Sprintf("unknown subcommand %q", sub))
	}

	fs := flag.NewFlagSet("keys "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Only create takes flags; list and revoke reject any.
	name, scope := new(string), new(string)
	if sub == "create" {
		fs.StringVar(name, "name", "", "key name (1-64 printable characters)")
		fs.StringVar(scope, "scope", "", "admin or analyze")
	}
	if err := fs.Parse(rest); err != nil {
		return usage(err.Error())
	}
	pos := fs.Args()
	switch {
	case sub == "revoke" && len(pos) != 1:
		return usage("revoke takes exactly one key id")
	case sub != "revoke" && len(pos) != 0:
		return usage(fmt.Sprintf("unexpected argument %q", pos[0]))
	case sub == "create" && (*name == "" || *scope == ""):
		return usage("create needs --name and --scope")
	}

	dataDir := strings.TrimSpace(getenv("DATA_DIR"))
	if dataDir == "" {
		fmt.Fprintln(stderr, "regressionmap keys: DATA_DIR is required (the directory that holds keys.json)")
		return 1
	}
	store := apikeys.NewStore(dataDir)

	switch sub {
	case "create":
		k, secret, err := store.Create(*name, *scope)
		if err != nil {
			fmt.Fprintln(stderr, "regressionmap keys:", err)
			return 1
		}
		fmt.Fprintln(stdout, secret)
		fmt.Fprintf(stderr, "Created %s key %s (%q). The secret above is shown only once; store it now.\nUse it as: Authorization: Bearer <secret>\n", k.Scope, k.ID, k.Name)
		return 0
	case "revoke":
		if err := store.Revoke(pos[0]); err != nil {
			if errors.Is(err, apikeys.ErrNotFound) {
				fmt.Fprintf(stderr, "regressionmap keys: key %q not found\n", pos[0])
			} else {
				fmt.Fprintln(stderr, "regressionmap keys:", err)
			}
			return 1
		}
		fmt.Fprintf(stderr, "Revoked key %s.\n", pos[0])
		return 0
	}

	keys, err := store.List()
	if err != nil {
		fmt.Fprintln(stderr, "regressionmap keys:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCOPE\tPREFIX\tCREATED_AT\tLAST_USED_AT\tREVOKED")
	for _, k := range keys {
		used := "never"
		if k.LastUsedAt != nil {
			used = k.LastUsedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n", k.ID, k.Name, k.Scope, k.Prefix, k.CreatedAt.UTC().Format(time.RFC3339), used, k.Revoked)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, "regressionmap keys:", err)
		return 1
	}
	return 0
}
