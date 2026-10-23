package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/app"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/rpc"
	"github.com/SayyakuHajime/whatsfeather/backend/internal/transport/whatsapp"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "whatsfeatherd:", e)
		os.Exit(1)
	}
}
func run() error {
	syscall.Umask(0077)
	sock := flag.String("socket", "", "Private Unix socket path")
	dir := flag.String("data-dir", "", "Private profile directory")
	demo := flag.Bool("demo", false, "Use synthetic local data (default)")
	live := flag.Bool("live", false, "Enable optional WhatsApp transport; still requires explicit connect")
	liveTestScopePath := flag.String("live-test-scope", "", "Restrict live input to a private bounded scope file")
	flag.Parse()
	scopeFlagSet := false
	flag.Visit(func(option *flag.Flag) {
		if option.Name == "live-test-scope" {
			scopeFlagSet = true
		}
	})
	if *demo && *live {
		return fmt.Errorf("demo and live are mutually exclusive")
	}
	var liveTestScope *whatsapp.LiveTestScope
	if scopeFlagSet {
		if !*live || *demo {
			return fmt.Errorf("live-test scope requires explicit --live mode")
		}
		var err error
		liveTestScope, err = whatsapp.LoadLiveTestScope(*liveTestScopePath)
		if err != nil {
			return err
		}
	}
	if *dir == "" || *sock == "" {
		return fmt.Errorf("--data-dir and --socket are required")
	}
	if !filepath.IsAbs(*dir) {
		return fmt.Errorf("data-dir must be absolute")
	}
	if liveTestScope != nil {
		var err error
		*dir, err = whatsapp.PrepareScopedLiveTestProfile(*dir, liveTestScope)
		if err != nil {
			return err
		}
	} else if err := whatsapp.RejectOrdinaryProfileMode(*dir); err != nil {
		return err
	}
	if e := os.MkdirAll(*dir, 0700); e != nil {
		return e
	}
	if st, e := os.Lstat(*dir); e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe profile directory")
	}
	lock, e := os.OpenFile(filepath.Join(*dir, "profile.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("profile is already in use")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	a, e := app.Open(*dir, !*live)
	if e != nil {
		return e
	}
	defer a.Close()
	if *live {
		if liveTestScope == nil {
			a.SetTransport(whatsapp.New(a, *dir))
		} else {
			a.SetTransport(whatsapp.NewWithLiveTestScope(a, *dir, liveTestScope))
		}
	}
	s, e := rpc.Listen(*sock, a)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return s.Run(ctx)
}
