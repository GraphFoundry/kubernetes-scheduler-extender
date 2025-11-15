package main

import (
	"log"
	"scheduler-extender/internal/app"
	"scheduler-extender/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	if err := app.New(cfg).Run(); err != nil {
		log.Fatal(err)
	}
}
