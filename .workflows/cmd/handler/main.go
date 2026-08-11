// Command handler boots the Restate handler HTTP service. It binds the
// workflow service definitions and serves them so the local
// restate-server can invoke them.
package main

import (
	"context"
	"os"

	"github.com/restatedev/sdk-go/server"

	"github.com/trento-project/trento-workflows/internal/lib"
	"github.com/trento-project/trento-workflows/internal/workflows/dependabotsweep"
	"github.com/trento-project/trento-workflows/internal/workflows/fixprci"
	"github.com/trento-project/trento-workflows/internal/workflows/patchrelease"
	"github.com/trento-project/trento-workflows/internal/workflows/testonobs"
)

func main() {
	log := lib.NewLogger()

	addr := os.Getenv("HANDLER_ADDR")
	if addr == "" {
		addr = ":9080"
	}

	endpoint := server.NewRestate().
		Bind(testonobs.Register()).
		Bind(testonobs.RegisterSubmoduleService()).
		Bind(patchrelease.Register()).
		Bind(patchrelease.RegisterRepoService()).
		Bind(fixprci.Register()).
		Bind(dependabotsweep.Register()).
		Bind(dependabotsweep.RegisterRepoService())

	log.Info("starting handler", "addr", addr)
	if err := endpoint.Start(context.Background(), addr); err != nil {
		log.Error("handler exited", "err", err)
		os.Exit(1)
	}
}
