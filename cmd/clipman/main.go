package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gioui.org/app"
	"gioui.org/unit"

	"clipman/internal/cliphist"
	"clipman/internal/clipboard"
	"clipman/internal/pins"
	"clipman/internal/ui"
)

func main() {
	// Gio requires app.Main() to run on the main OS thread, and app.Main() does
	// not return on desktop platforms. Run all application lifecycle and deferred
	// cleanup inside run() before exiting with its returned status code.
	go func() {
		os.Exit(run())
	}()
	app.Main()
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var pinStore *pins.Store
	var initErr error

	pinDir, err := pins.DefaultDir()
	if err != nil {
		initErr = fmt.Errorf("pin storage path unavailable: %w", err)
	} else {
		pinStore, err = pins.Open(pinDir)
		if errors.Is(err, pins.ErrAlreadyLocked) {
			fmt.Fprintln(os.Stderr, "clipman: another instance is already running (pin storage is locked)")
			return 1
		}
		if pinStore != nil {
			defer func() {
				if closeErr := pinStore.Close(); closeErr != nil {
					fmt.Fprintf(os.Stderr, "clipman: close pin store: %v\n", closeErr)
				}
			}()
		}
		if err != nil {
			// Preserve corruption, version, or read-only recovery warnings for the UI.
			initErr = err
		}
	}

	history := cliphist.NewClient()
	copier := clipboard.NewCopier()
	state := ui.NewState(history, copier, pinStore, initErr)

	w := new(app.Window)
	w.Option(
		app.Title("clipman"),
		app.Size(unit.Dp(580), unit.Dp(620)),
		app.MinSize(unit.Dp(420), unit.Dp(360)),
	)

	gui := ui.NewApp(ctx, w, state)
	if err := gui.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "clipman: %v\n", err)
		return 1
	}
	return 0
}
