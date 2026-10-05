package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

const userUsage = `usage:
  ledger user add <name> [--password-stdin]
  ledger user passwd <name> [--password-stdin]
  ledger user disable <name>
  ledger user enable <name>
  ledger user list`

// runUserCommand implements `ledger user ...`. It uses the same DATABASE_URL
// as the server.
func runUserCommand(args []string) error {
	if len(args) == 0 {
		return errors.New(userUsage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet("user "+cmd, flag.ContinueOnError)
	fromStdin := fs.Bool("password-stdin", false, "read the password from standard input")
	// Allow the flag before or after the name.
	var name string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		if len(rest) > 0 {
			if name != "" {
				return errors.New(userUsage)
			}
			name, rest = rest[0], rest[1:]
		}
	}
	if (cmd == "list") != (name == "") {
		return errors.New(userUsage)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := connectDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	app := NewApp(pool, cfg)

	switch cmd {
	case "add":
		pw, err := readPassword(*fromStdin)
		if err != nil {
			return err
		}
		u, err := app.CreateUser(ctx, name, pw)
		if err != nil {
			return cliError(err)
		}
		fmt.Printf("created user %s\n", u.Username)
	case "passwd":
		pw, err := readPassword(*fromStdin)
		if err != nil {
			return err
		}
		if err := app.SetPassword(ctx, name, pw); err != nil {
			return cliError(err)
		}
		fmt.Printf("password updated for %s; their sessions were ended\n", normalizeUsername(name))
	case "disable", "enable":
		if err := app.SetUserActive(ctx, name, cmd == "enable"); err != nil {
			return cliError(err)
		}
		fmt.Printf("user %s %sd\n", normalizeUsername(name), cmd)
	case "list":
		users, err := app.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USERNAME\tACTIVE\tCREATED\tLAST LOGIN")
		for _, u := range users {
			last := "never"
			if u.LastLoginAt != nil {
				last = u.LastLoginAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(tw, "%s\t%t\t%s\t%s\n", u.Username, u.IsActive, u.CreatedAt.Local().Format("2006-01-02"), last)
		}
		return tw.Flush()
	default:
		return errors.New(userUsage)
	}
	return nil
}

func cliError(err error) error {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return errors.New(strings.TrimPrefix(ve.Error(), "validation failed: "))
	case errors.Is(err, ErrNotFound):
		return errors.New("no such user")
	}
	if strings.Contains(err.Error(), "users_username_unique") {
		return errors.New("a user with that name already exists")
	}
	return err
}

// readPassword prompts twice on a terminal (no echo), or reads one line from
// stdin with --password-stdin.
func readPassword(fromStdin bool) (string, error) {
	if fromStdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	p2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(p1) != string(p2) {
		return "", errors.New("passwords do not match")
	}
	return string(p1), nil
}
