// Command migrate applies the embedded SQL migrations to DATABASE_URL.
//
//	migrate up           apply all pending migrations
//	migrate down [N|all] roll back N migrations (default 1), or all
//	migrate version      print the current version
//	migrate force V      set the version without running SQL (fix a dirty state)
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"

	"github.com/dezmymachine/farmish-backend/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) (err error) {
	if len(args) == 0 {
		return errors.New("usage: migrate up | down [N|all] | version | force V")
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	m, err := migrations.New(dbURL)
	if err != nil {
		return err
	}
	defer func() {
		srcErr, dbErr := m.Close()
		err = errors.Join(err, srcErr, dbErr)
	}()

	switch args[0] {
	case "up":
		err = m.Up()
	case "down":
		switch {
		case len(args) < 2:
			err = m.Steps(-1)
		case args[1] == "all":
			err = m.Down()
		default:
			n, convErr := strconv.Atoi(args[1])
			if convErr != nil || n < 1 {
				return fmt.Errorf("down: N must be a positive integer or \"all\", got %q", args[1])
			}
			err = m.Steps(-n)
		}
	case "version":
		v, dirty, vErr := m.Version()
		if errors.Is(vErr, migrate.ErrNilVersion) {
			fmt.Println("no migrations applied")
			return nil
		}
		if vErr != nil {
			return vErr
		}
		fmt.Printf("version %d (dirty: %t)\n", v, dirty)
		return nil
	case "force":
		if len(args) < 2 {
			return errors.New("force: version required")
		}
		v, convErr := strconv.Atoi(args[1])
		if convErr != nil {
			return fmt.Errorf("force: bad version %q", args[1])
		}
		err = m.Force(v)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}

	if errors.Is(err, migrate.ErrNoChange) {
		fmt.Println("no change")
		return nil
	}
	if err == nil {
		fmt.Println("ok")
	}
	return err
}
