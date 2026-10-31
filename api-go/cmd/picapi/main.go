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

	"picapi/internal/client"
)

const usage = `picapi: command-line client for a picapi server

Usage:
  picapi login --url URL --key KEY         save and verify credentials
  picapi upload [--name NAME] FILE...
  picapi ls [--limit N] [--all]
  picapi info ID
  picapi rm ID...
  picapi url ID [-t TRANSFORMS] [--slug SLUG] [--expires DURATION]

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
		for _, path := range fs.Args() {
			asset, err := c.Upload(path, *name)
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
		fmt.Fprintln(w, "ID\tSIZE\tDIMENSIONS\tCREATED\tFILENAME")
		for _, a := range assets {
			fmt.Fprintf(w, "%s\t%s\t%dx%d\t%s\t%s\n", a.ID, humanBytes(a.SizeBytes), a.Width, a.Height, a.CreatedAt[:10], a.Filename)
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

	case "url":
		transforms := fs.String("t", "", "transforms, e.g. w_320,f_webp")
		slug := fs.String("slug", "", "readable file name, e.g. photo.webp")
		expires := fs.Duration("expires", 0, "expiry, e.g. 24h")
		if err := parse(fs, reorder(args), 1); err != nil {
			return err
		}
		c, err := loadClient()
		if err != nil {
			return err
		}
		u, err := c.URL(fs.Arg(0), *transforms, *slug, int64(expires.Seconds()))
		if err != nil {
			return err
		}
		fmt.Println(u)
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

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
