package main

import "github.com/laurisb/osm2mysql/internal/osm2mysql"

// version is overridden at release build time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	osm2mysql.Run(version)
}
