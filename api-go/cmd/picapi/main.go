// Command picapi is the command-line client for a picapi server.
//
//	picapi login --url https://img.example.com --key pk_...
//	picapi upload photo.jpg
//	picapi url <id> -t w_320,f_webp
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"picapi/internal/client"
)

const usage = `picapi: command-line client for a picapi server

Usage:
  picapi login --url URL --key KEY         save and verify credentials
  picapi upload [--private] [--name NAME] FILE...
  picapi ls [--limit N] [--all]
  picapi info ID
  picapi set ID [--name NAME] [--visibility public|private]
  picapi rm ID...
  picapi url ID [-t TRANSFORMS] [--slug SLUG] [--expires DURATION]
  picapi share ID [-t TRANSFORMS] [--expires DURATION]
  picapi keys [list | create NAME | revoke ID]
  picapi reconcile [--fix]

Transforms are comma-separated, e.g. w_320,f_webp or p_thumb.
Credentials come from PICAPI_URL and PICAPI_KEY, or from the file written by login.
Add --json after the command name for machine-readable output.
`

type config struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "picapi", "config.json"), nil
}

func loadClient() (*client.Client, error) {
	cfg := config{URL: os.Getenv("PICAPI_URL"), Key: os.Getenv("PICAPI_KEY")}
	if cfg.URL == "" || cfg.Key == "" {
		path, err := configPath()
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("not logged in; run `picapi login --url URL --key KEY` or set PICAPI_URL and PICAPI_KEY")
		}
		var saved config
		if err := json.Unmarshal(data, &saved); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if cfg.URL == "" {
			cfg.URL = saved.URL
		}
		if cfg.Key == "" {
			cfg.Key = saved.Key
		}
	}
	return client.New(cfg.URL, cfg.Key), nil
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "picapi:", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")

	switch command {
	case "login":
		url := fs.String("url", "", "server URL")
		key := fs.String("key", "", "API key")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *url == "" || *key == "" {
			return errors.New("login needs --url and --key")
		}
		if _, err := client.New(*url, *key).Session(); err != nil {
			return err
		}
		path, err := configPath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		data, _ := json.MarshalIndent(config{URL: strings.TrimRight(*url, "/"), Key: *key}, "", "  ")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
		fmt.Println("Logged in. Credentials saved to", path)
		return nil

	case "upload":
		private := fs.Bool("private", false, "upload as private")
		name := fs.String("name", "", "filename to store (single file only)")
		if err := parse(fs, args, 1); err != nil {
			return err
		}
		if *name != "" && fs.NArg() > 1 {
			return errors.New("--name works with a single file")
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		visibility := "public"
		if *private {
			visibility = "private"
		}
		for _, path := range fs.Args() {
			asset, err := c.Upload(path, *name, visibility)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if *asJSON {
				printJSON(asset)
			} else {
				fmt.Printf("%s\t%s\n", asset.ID, asset.URL)
			}
		}
		return nil

	case "ls":
		limit := fs.Int("limit", 50, "page size (1-100)")
		all := fs.Bool("all", false, "list every asset")
		if err := parse(fs, args, 0); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		var assets []client.Asset
		cursor := ""
		for {
			page, err := c.List(*limit, cursor)
			if err != nil {
				return err
			}
			assets = append(assets, page.Items...)
			if !*all || page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		if *asJSON {
			printJSON(assets)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tVISIBILITY\tSIZE\tDIMENSIONS\tCREATED\tFILENAME")
		for _, a := range assets {
			fmt.Fprintf(w, "%s\t%s\t%s\t%dx%d\t%s\t%s\n", a.ID, a.Visibility, humanBytes(a.SizeBytes), a.Width, a.Height, a.CreatedAt[:10], a.Filename)
		}
		return w.Flush()

	case "info":
		if err := parse(fs, args, 1); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		asset, err := c.Get(fs.Arg(0))
		if err != nil {
			return err
		}
		printJSON(asset)
		return nil

	case "set":
		name := fs.String("name", "", "new filename")
		visibility := fs.String("visibility", "", "public or private")
		if err := parse(fs, reorder(args), 1); err != nil {
			return err
		}
		fields := map[string]string{}
		if *name != "" {
			fields["filename"] = *name
		}
		if *visibility != "" {
			fields["visibility"] = *visibility
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		asset, err := c.Update(fs.Arg(0), fields)
		if err != nil {
			return err
		}
		printJSON(asset)
		return nil

	case "rm":
		if err := parse(fs, args, 1); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		for _, id := range fs.Args() {
			if err := c.Delete(id); err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
			fmt.Println("deleted", id)
		}
		return nil

	case "url", "share":
		transforms := fs.String("t", "", "transforms, e.g. w_320,f_webp")
		slug := fs.String("slug", "", "readable file name, e.g. photo.webp (url only)")
		expires := fs.Duration("expires", 0, "expiry, e.g. 24h")
		if err := parse(fs, reorder(args), 1); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		seconds := int64(expires.Seconds())
		if command == "url" {
			u, err := c.URL(fs.Arg(0), *transforms, *slug, seconds)
			if err != nil {
				return err
			}
			fmt.Println(u)
			return nil
		}
		share, err := c.Share(fs.Arg(0), *transforms, seconds)
		if err != nil {
			return err
		}
		if *asJSON {
			printJSON(share)
		} else {
			fmt.Println(share.URL)
			fmt.Fprintln(os.Stderr, "expires", share.ExpiresAt)
		}
		return nil

	case "keys":
		if err := fs.Parse(args); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		sub := fs.Arg(0)
		switch {
		case sub == "" || sub == "list":
			keys, err := c.Keys()
			if err != nil {
				return err
			}
			if *asJSON {
				printJSON(keys)
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tCREATED\tLAST USED\tSTATUS")
			for _, k := range keys {
				status := "active"
				if k.RevokedAt != nil {
					status = "revoked"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.CreatedAt[:10], deref(k.LastUsed), status)
			}
			return w.Flush()
		case sub == "create" && fs.NArg() == 2:
			key, err := c.CreateKey(fs.Arg(1))
			if err != nil {
				return err
			}
			if *asJSON {
				printJSON(key)
				return nil
			}
			fmt.Println(key.Key)
			fmt.Fprintln(os.Stderr, "Store this key now; it won't be shown again. ID:", key.ID)
			return nil
		case sub == "revoke" && fs.NArg() == 2:
			if err := c.RevokeKey(fs.Arg(1)); err != nil {
				return err
			}
			fmt.Println("revoked", fs.Arg(1))
			return nil
		}
		return errors.New("usage: picapi keys [list | create NAME | revoke ID]")

	case "reconcile":
		fix := fs.Bool("fix", false, "delete orphans and stale rows")
		if err := parse(fs, args, 0); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		report, err := c.Reconcile(*fix)
		if err != nil {
			return err
		}
		if *asJSON {
			printJSON(report)
			return nil
		}
		verb := "found"
		if report.Fixed {
			verb = "fixed"
		}
		fmt.Printf("orphan originals:   %d %s\n", len(report.OrphanOriginals), verb)
		fmt.Printf("orphan variants:    %d %s\n", len(report.OrphanVariants), verb)
		fmt.Printf("stale variant rows: %d %s\n", len(report.StaleVariantRows), verb)
		fmt.Printf("missing originals:  %d (cannot be fixed automatically)\n", len(report.MissingOriginals))
		for _, id := range report.MissingOriginals {
			fmt.Println("  missing:", id)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q; run `picapi help`", command)
}

// parse parses flags and requires at least minArgs positional arguments.
func parse(fs *flag.FlagSet, args []string, minArgs int) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < minArgs {
		return fmt.Errorf("%s needs %d argument(s); run `picapi help`", fs.Name(), minArgs)
	}
	return nil
}

// reorder moves a leading positional ID behind the flags, so `picapi url ID -t w_320` works.
func reorder(args []string) []string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return append(append([]string{}, args[1:]...), args[0])
	}
	return args
}

func printJSON(v any) {
	data, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(data))
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	if t, err := time.Parse(time.RFC3339, *s); err == nil {
		return t.Format("2006-01-02")
	}
	return *s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
