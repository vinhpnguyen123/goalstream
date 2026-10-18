// Command ingest cuts goal clips from SoccerNet matches into object storage and Postgres.
//
//	ingest migrate   apply pending schema migrations (the same code the API runs at startup)
package main

import (
	"fmt"
	"os"

	"github.com/vinhpnguyen123/goalstream/internal/db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ingest:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ingest migrate")
	}
	switch args[0] {
	case "migrate":
		return db.Migrate(os.Getenv("DATABASE_URL"))
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}
