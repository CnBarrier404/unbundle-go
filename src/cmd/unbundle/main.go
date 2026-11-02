package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"unbundle-go/src/internal/config"
	"unbundle-go/src/internal/messages"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out, diagnostics io.Writer) error {
	usage := "usage: unbundle config {example|check|show} [--config FILE]\n       unbundle request check --config FILE < request.json\nThese commands never run inference."
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	if len(args) < 2 {
		return errors.New(usage)
	}
	if args[0] == "config" && args[1] == "example" {
		if len(args) != 2 {
			return errors.New(usage)
		}
		c := config.Defaults()
		c.TokenFile = "gateway-token"
		c.DefaultModel = "claude-sonnet-5-5"
		// Example routing only: account access and model capabilities are unverified.
		c.Models = []config.Model{{ID: c.DefaultModel, DisplayName: "Claude Sonnet 5.5", CLIModel: c.DefaultModel, OutputTokenLimit: 8192}}
		return writeJSON(out, c)
	}
	if !(args[0] == "config" && (args[1] == "check" || args[1] == "show") || args[0] == "request" && args[1] == "check") {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet(args[0]+" "+args[1], flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	name := flags.String("config", "", "explicit gateway JSON configuration file")
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *name == "" || flags.NArg() != 0 {
		return errors.New("--config FILE is required; positional arguments are not accepted")
	}
	c, err := config.LoadFile(*name)
	if err != nil {
		return err
	}
	if args[0] == "request" {
		if _, err := messages.Decode(in, c); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "request structure and deployment limits are valid; no inference was performed")
		return err
	}
	if args[1] == "show" {
		return writeJSON(out, c)
	}
	_, err = fmt.Fprintln(out, "configuration is valid; CLI, credentials, account access and port availability were not checked")
	return err
}

func writeJSON(out io.Writer, value any) error {
	if err := json.MarshalWrite(out, value, jsontext.WithIndent("  ")); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out)
	return err
}
