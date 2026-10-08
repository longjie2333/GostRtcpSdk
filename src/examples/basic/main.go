// A host application owns configuration, cancellation and error handling.
package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"os/signal"

	rtcp "github.com/longjie2333/GostRtcpSdk/src"
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
	client, err := rtcp.NewClient(cfg, target)
	if err != nil {
		log.Fatal(err)
	}
	if err := client.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
