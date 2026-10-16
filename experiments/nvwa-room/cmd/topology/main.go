package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/relaishq/relais/experiments/nvwa-room/harness"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	bin := flag.String("bin", "", "spike binary directory")
	dir := flag.String("out", "", "private process/evidence directory")
	data := flag.Bool("datachannel", false, "requires W1 integration")
	flag.Parse()
	root, _ := os.Getwd()
	b, _ := filepath.Abs(*bin)
	d, _ := filepath.Abs(*dir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t, e := harness.Start(ctx, b, root, d, *data)
	cancel()
	if e != nil {
		log.Fatal(e)
	}
	defer t.Close()
	_ = json.NewEncoder(os.Stdout).Encode(t.Config)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	<-stop
}
