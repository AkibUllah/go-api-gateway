// Command gateway runs the API gateway.
//
//	gateway -config configs/gateway.yaml                 run
//	gateway -config configs/gateway.yaml -check          validate the config and exit
//	gateway -healthcheck http://127.0.0.1:8080/healthz   probe (container HEALTHCHECK)
//
// SIGHUP reloads the config file; SIGINT/SIGTERM shut down gracefully.
package main

import (
	"os"

	"github.com/AkibUllah/go-api-gateway/internal/app"
)

func main() {
	os.Exit(app.Run(os.Args[1:]))
}
