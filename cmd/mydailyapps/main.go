// MyDailyApps: a launcher host for your own apps. It shows each app as a
// card, installs it on request, starts it and opens it in a new browser tab.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robiwizzard/mydailyapps/internal/api"
	"github.com/robiwizzard/mydailyapps/internal/config"
	"github.com/robiwizzard/mydailyapps/internal/host"
	"github.com/robiwizzard/mydailyapps/internal/procs"
	"github.com/robiwizzard/mydailyapps/internal/state"
	"github.com/robiwizzard/mydailyapps/ui"
)

// Version is set at build time with -ldflags "-X main.Version=v1.2.3".
var Version = "dev"

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("MyDailyApps", Version)
		return
	}
	if err := run(); err != nil {
		log.Printf("error: %v", err)
		if runtime.GOOS == "windows" {
			// Keep the console window open long enough to read the error.
			fmt.Println("\nPress Enter to close.")
			fmt.Scanln()
		}
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Printf("MyDailyApps %s starting (%s mode)", Version, cfg.Mode)

	// The state file prevents a second instance and reveals a crashed session.
	sf, prev, err := procs.AcquireState(procs.DefaultStatePath(), procs.HostState{Port: cfg.Port, Mode: string(cfg.Mode)})
	var running *procs.AlreadyRunningError
	if errors.As(err, &running) {
		url := fmt.Sprintf("http://localhost:%d", running.Port)
		log.Printf("MyDailyApps is already running at %s; opening it.", url)
		if cfg.OpenBrowser {
			openBrowser(url)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	var notices []string
	if prev != nil {
		msg := "The last session ended unexpectedly."
		if stopped := procs.CleanupLeftovers(prev); len(stopped) > 0 {
			msg += " Stopped leftover apps: " + strings.Join(stopped, ", ") + "."
		}
		if prev.GuestDir != "" {
			_ = config.RemoveAllRetry(prev.GuestDir)
		}
		log.Print(msg)
		notices = append(notices, msg)
	}
	// Guest folders from crashed sessions must not be left behind.
	if removed := config.CleanGuestLeftovers(procs.IsAlive); len(removed) > 0 {
		log.Printf("removed %d leftover guest folder(s)", len(removed))
	}

	if err := cfg.Prepare(); err != nil {
		sf.Remove()
		return err
	}
	_ = sf.SetGuestDir(cfg.GuestDir)
	cleanupFolders := func() {
		if err := cfg.Cleanup(); err != nil {
			log.Printf("could not remove the guest folder %s: %v", cfg.GuestDir, err)
		}
	}
	log.Printf("data folder: %s", cfg.DataDir)
	log.Printf("apps folder: %s", cfg.AppsDir)

	store, err := state.Open(cfg.DataDir)
	if err != nil {
		cleanupFolders()
		sf.Remove()
		return fmt.Errorf("local state: %w", err)
	}

	h, err := host.New(cfg, store, sf)
	if err != nil {
		store.Close()
		cleanupFolders()
		sf.Remove()
		return err
	}
	for _, n := range notices {
		h.AddNotice(n)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.Port))
	if err != nil {
		store.Close()
		cleanupFolders()
		sf.Remove()
		return fmt.Errorf("port %d is not available (set MYDAILYAPPS_PORT to use another): %w", cfg.Port, err)
	}
	srv := &http.Server{Handler: api.Handler(h, ui.FS(), cfg.Port), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	// Browsers may resolve "localhost" to ::1 first; listen there too if we can.
	if ln6, err := net.Listen("tcp", "[::1]:"+strconv.Itoa(cfg.Port)); err == nil {
		go srv.Serve(ln6)
	}

	url := fmt.Sprintf("http://localhost:%d", cfg.Port)
	log.Printf("open %s (Ctrl+C to shut down)", url)
	ctx, cancel := context.WithCancel(context.Background())
	go h.Startup(ctx)
	if cfg.OpenBrowser {
		openBrowser(url)
	}

	// Ctrl+C, closing the console window, logoff and system shutdown all arrive
	// here (on Windows they are delivered as os.Interrupt / SIGTERM).
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	fromSignal := false
	select {
	case s := <-sig:
		log.Printf("received %v, shutting down", s)
		fromSignal = true
	case <-h.ShutdownCh:
		log.Print("shutdown requested from the UI")
	}

	// One shutdown path for every case.
	cancel()
	if fromSignal {
		// Windows gives a closing console only a few seconds.
		procs.StopGrace = 2 * time.Second
	}
	h.Procs.StopAll(false)
	// Apps upload their changed files after they exit; give that time to finish.
	syncWait := 60 * time.Second
	if fromSignal {
		syncWait = 4 * time.Second
	}
	if !h.WaitSyncs(syncWait) {
		log.Print("some cloud uploads did not finish; they will be uploaded after the app's next run")
	}
	shutCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(shutCtx)
	done()
	store.Close()
	cleanupFolders()
	_ = sf.Remove()
	log.Print("MyDailyApps has shut down")
	return nil
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	if err := c.Start(); err != nil {
		log.Printf("could not open the browser (%v); open %s yourself", err, url)
		return
	}
	go c.Wait()
}
