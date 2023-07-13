// Package main provides catp CLI tool.
package main

import (
	"log"

	"github.com/bool64/progress/cmd/catp/app"
)

func main() {
	if err := app.Main(); err != nil {
		log.Fatal(err)
	}
}
