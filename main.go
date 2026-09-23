package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg := LoadConfig()
	srv := NewServer(cfg)
	httpSrv := srv.Start()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down...")
	srv.Shutdown(httpSrv)
}
