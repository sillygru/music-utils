package main

import (
	"os"

	"github.com/sillygru/music-utils/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
