package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sync/errgroup"
)

// This is a helper program to run the controller and the source controller port forward in parallel for local development.
func main() {
	g, ctx := errgroup.WithContext(context.Background())
	g.Go(func() error {
		fmt.Println("Running port forward to source controller")
		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", "source-system", "svc/source-controller", "8080:80")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})
	g.Go(func() error {
		fmt.Println("Running controller")
		cmd := exec.CommandContext(ctx, "go", "run", "main.go", "controller", "--source-controller-hostname-override=localhost:8080")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})

	if err := g.Wait(); err != nil {
		os.Exit(1)
	}
}
