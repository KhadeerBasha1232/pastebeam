// pastebeam sends files between machines with a short code.
//
//	pastebeam send file.zip      -> prints a code like 7-tiger-lamp
//	pastebeam get 7-tiger-lamp   -> receives it, end-to-end encrypted
//
// No server to run: peers find each other via mDNS (LAN) or the public
// libp2p DHT (internet), connect directly (hole punching through NAT),
// and agree on a key from the code with SPAKE2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	logging "github.com/ipfs/go-log/v2"
)

var version = "dev"

func printUsage() {
	header("send files between machines with a short code")
	cmd := func(c, desc string) {
		out.Println("    " + sAccent.Render(fmt.Sprintf("%-38s", c)) + sDim.Render(desc))
	}
	out.Println("  " + sBold.Render("Usage"))
	cmd("pastebeam send <file|folder>", "share it, prints a code like 7-tiger-lamp")
	cmd("pastebeam get <code>", "receive it on the other machine")
	cmd("pastebeam version", "print the version")
	blank()
	out.Println("  " + sBold.Render("Flags"))
	cmd("send --code CODE", "use your own code instead of a random one")
	cmd("get  --out DIR", "where to save (default: current folder)")
	cmd("get  --yes, -y", "accept without asking")
	cmd("get  --timeout 3m", "how long to look for the sender")
	cmd("--verbose, -v", "show libp2p network logs")
	blank()
	out.Println("  " + sDim.Render("No server needed: LAN via mDNS, internet via the public libp2p network."))
	blank()
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			err = errors.New("cancelled")
		}
		failLine(err)
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.Usage = printUsage
	verbose := fs.Bool("verbose", false, "")
	fs.BoolVar(verbose, "v", false, "")

	switch args[0] {
	case "send":
		code := fs.String("code", "", "")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: pastebeam send <file|folder>")
		}
		quiet(*verbose)
		return runSend(ctx, pos[0], *code)

	case "get", "receive", "recv":
		out := fs.String("out", ".", "")
		yes := fs.Bool("yes", false, "")
		fs.BoolVar(yes, "y", false, "")
		timeout := fs.Duration("timeout", 3*time.Minute, "")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: pastebeam get <code>")
		}
		quiet(*verbose)
		return runGet(ctx, pos[0], *out, *yes, *timeout)

	case "version", "--version":
		fmt.Println("pastebeam", version)
		return nil

	case "help", "-h", "--help":
		printUsage()
		return nil
	}
	return fmt.Errorf("unknown command %q (try: send, get)", args[0])
}

// parse allows flags before or after the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func quiet(verbose bool) {
	if verbose {
		logging.SetAllLoggers(logging.LevelInfo)
		return
	}
	logging.SetAllLoggers(logging.LevelFatal)
	os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
}
