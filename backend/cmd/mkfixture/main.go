// Command mkfixture creates the RegressionMap demo fixture repository.
//
//	go run ./cmd/mkfixture <workspace-dir>
//
// It creates <workspace-dir>/pagination-demo. With -fix it instead writes the
// known fix into an existing fixture's working tree.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"regressionmap/internal/fixture"
)

func main() {
	fix := flag.Bool("fix", false, "apply the known fix to an existing fixture working tree")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mkfixture [-fix] <workspace-dir>")
		os.Exit(2)
	}
	dir := filepath.Join(flag.Arg(0), "pagination-demo")
	if *fix {
		if err := fixture.ApplyFix(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("fix written to", filepath.Join(dir, "src", "pagination.ts"))
		return
	}
	s, err := fixture.Build(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("created %s\nbaseline %s\nrefactor %s\nbug      %s\n", dir, s.Baseline, s.Refactor, s.Bug)
}
