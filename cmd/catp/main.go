// Package main provides catp CLI tool.
package main

import (
	"log"

	"github.com/bool64/progress/cmd/catp/catp"
)

func main() {
	if err := catp.Main(); err != nil {
		log.Fatal(err)
	}
}
