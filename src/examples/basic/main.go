// A host application owns configuration, cancellation and error handling.
package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"os/signal"

	rtcp "example.com/gostrtcpsdk/src"
)

func main() {
	server, bind, target := os.Getenv("GOST_SERVER"), os.Getenv("GOST_BIND"), os.Getenv("GOST_TARGET")
	if server == "" || bind == "" || target == "" {
		log.Fatal("set GOST_SERVER, GOST_BIND and GOST_TARGET")
	}
	cfg := rtcp.Config{Server: server, Bind: bind}
	if user, password := os.Getenv("GOST_USER"), os.Getenv("GOST_PASSWORD"); user != "" || password != "" {
		cfg.User = url.UserPassword(user, password)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := rtcp.Run(ctx, cfg, target); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
