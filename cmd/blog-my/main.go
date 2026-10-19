// Command blog-my starts the desktop application HTTP backend.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"blog-my/internal/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
