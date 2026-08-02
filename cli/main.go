// sendup — publish static pages to a subdomain of a domain you control.
package main

import (
	"fmt"
	"os"

	"sendup/internal/command"
	"sendup/internal/config"
)

var commands = map[string]func([]string) error{
	"publish": command.Publish,
	"p":       command.Publish,
	"list":    command.List,
	"ls":      command.List,
	"rm":      command.Remove,
	"delete":  command.Remove,
	"rename":  command.Rename,
	"mv":      command.Rename,
	"link":    command.Link,
	"pull":    command.Pull,
	"sync":    command.Pull,
	"doctor":  command.Doctor,
	"check":   command.Doctor,
	"admin":   command.Admin,
	"token":   command.Token,
	"init":    command.Init,
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	name := os.Args[1]
	if name == "help" || name == "-h" || name == "--help" {
		usage()
		return
	}

	run, ok := commands[name]
	if !ok {
		usage()
		os.Exit(1)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "sendup: %v\n", err)
		os.Exit(1)
	}
}

// domainOr reports the configured domain, or a placeholder before `init` has run.
func domainOr(fallback string) string {
	cfg, err := config.Load()
	if err != nil || cfg.Domain == "" {
		return fallback
	}
	return cfg.Domain
}

func usage() {
	fmt.Printf(`sendup — publish static pages to *.%s

  sendup publish <file|dir> [--name NAME] [--ttl DAYS]
                            [--auth [user:pass]] [--no-auth]
                            [--link] [--link-ttl DAYS] [--no-link] [--rotate]
                            [--new]
  sendup link <slug> [--ttl DAYS]
  sendup list
  sendup rm <slug>
  sendup rename <slug> <new-name>
  sendup pull [file|dir]                         what changed on the server since you published
  sendup doctor [--fix]                          check local config against Cloudflare
  sendup admin [--user NAME] [--password PASS]   set up the dashboard
  sendup token <TOKEN>                           swap the stored API token
  sendup init --domain <your-domain> [--token TOKEN] [--skip-dns]

A single .html file becomes the site root. A directory is uploaded as-is;
index.html at its root becomes "/".

The subdomain defaults to 16 random hex chars. --name picks it by hand, e.g.
--name docs serves https://docs.<your-domain>/ (--slug is accepted as an alias).

Publishing the same file or directory again reuses the subdomain it went to
last time, so the URL you shared keeps working. Files removed since the last
publish are swept. --new forces a fresh subdomain.

Three ways to control who gets in, and they compose:

  (default)  the random subdomain IS the secret — anyone with the URL, no prompt
  --link     only a signed link opens the page; no password to type. --link-ttl
             sets an expiry, --rotate kills every link issued so far, and
             "sendup link <slug>" mints more without re-uploading
  --auth     HTTP Basic auth; bare --auth generates the credentials, prints once

With both --link and --auth, either one gets you in. A republish keeps whatever
the site already had; --no-auth and --no-link remove them.
`, domainOr("<your-domain>"))
}
