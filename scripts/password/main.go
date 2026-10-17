// SPDX-License-Identifier: AGPL-3.0-or-later

// Command password sets a local account's password.
//
//	make password                    # lists the local accounts
//	make password ARGS=-user adam    # prompts for a new password, twice
//
// "I have forgotten my password" is the most ordinary support request there
// is, and until this existed the answer on a self-hosted install was to open
// psql and write an argon2id hash by hand. SetLocalPassword has been in the
// store since authentication was built and had no caller anywhere: no command,
// no endpoint, nothing.
//
// It goes through the same hashing the sign-in path verifies against, rather
// than around it, so there is exactly one place that decides what a stored
// password looks like.
//
// The password is read from the terminal with echo off and is never taken as
// an argument: an argument is in the shell history, in the process list, and
// in any log that records the command.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/term"

	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

func main() {
	user := flag.String("user", "", "the account to set a password for")
	flag.Parse()

	if err := run(*user); err != nil {
		fmt.Fprintln(os.Stderr, "password:", err)
		os.Exit(1)
	}
}

func run(username string) error {
	ctx := context.Background()

	database := os.Getenv("DATABASE_URL")
	if database == "" {
		return errors.New("DATABASE_URL is not set; run `make password` rather than `go run`")
	}
	pool, err := postgres.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	accounts, err := localAccounts(ctx, pool)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return errors.New("there are no local accounts; a federated account's password lives in its directory")
	}

	if username == "" {
		fmt.Println("Local accounts:")
		for name, admin := range accounts {
			role := "reader"
			if admin {
				role = "administrator"
			}
			fmt.Printf("  %-24s %s\n", name, role)
		}
		fmt.Println("\nPick one: make password ARGS='-user <name>'")
		return nil
	}

	id, ok := accounts[username]
	_ = id
	if !ok {
		return fmt.Errorf("%q has no local account; `make password` lists the ones that do", username)
	}

	password, err := askTwice()
	if err != nil {
		return err
	}

	userID, err := idOf(ctx, pool, username)
	if err != nil {
		return err
	}
	if err := pool.SetLocalPassword(ctx, userID, password); err != nil {
		return err
	}

	fmt.Printf("\n%s can sign in with the new password.\n", username)
	fmt.Println("Every existing session stays valid; end them from Account if that matters.")
	return nil
}

// askTwice reads the new password without echoing it, and asks again to catch
// a typo - which on a password nobody can read back is otherwise discovered by
// being locked out.
func askTwice() (string, error) {
	if !term.IsTerminal(int(syscall.Stdin)) {
		// Piped input: read one line and trust it. Refusing would make this
		// unusable from a provisioning script, and echoing is not a concern
		// when there is no terminal to echo to.
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}

	fmt.Print("New password: ")
	first, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading the password: %w", err)
	}

	fmt.Print("Again: ")
	second, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading the password: %w", err)
	}

	if string(first) != string(second) {
		return "", errors.New("those did not match; nothing was changed")
	}
	return string(first), nil
}

func localAccounts(ctx context.Context, pool *postgres.Pool) (map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		SELECT u.username, u.is_admin
		FROM users u
		JOIN user_identities i ON i.user_id = u.id AND i.provider = 'local'
		ORDER BY u.username`)
	if err != nil {
		return nil, fmt.Errorf("reading accounts: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		var admin bool
		if err := rows.Scan(&name, &admin); err != nil {
			return nil, fmt.Errorf("scanning account: %w", err)
		}
		out[name] = admin
	}
	return out, rows.Err()
}

func idOf(ctx context.Context, pool *postgres.Pool, username string) (uuid.UUID, error) {
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM users WHERE lower(username) = lower($1)`, username).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("finding %q: %w", username, err)
	}
	return id, nil
}
