package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"golang.org/x/sync/errgroup"
)

// Namespaces differ per install: config/flux puts flux in chrysopoeia-flux-system, athanor
// in hel-flux. Look the services up by name rather than hardcoding either.
type serviceLookup struct {
	name     string
	envVar   string
	notFound string
	fallback string
}

var (
	sourceController = serviceLookup{
		name:     "source-controller",
		envVar:   "FLUX_NAMESPACE",
		notFound: "is flux installed? (kubectl apply -k config/flux)",
	}
	imageReflectorController = serviceLookup{
		name:     "image-reflector-controller-tags",
		envVar:   "FLUX_NAMESPACE",
		notFound: "is flux installed? (kubectl apply -k config/flux)",
	}
	chrysopoeia = serviceLookup{
		name:     "chrysopoeia-webhook-service",
		envVar:   "CHRYSOPOEIA_NAMESPACE",
		fallback: "default",
	}
)

func pickNamespace(l serviceLookup, out string) (string, error) {
	namespaces := strings.Fields(out)
	switch {
	case len(namespaces) == 1:
		return namespaces[0], nil
	case len(namespaces) == 0 && l.fallback != "":
		return l.fallback, nil
	case len(namespaces) == 0:
		return "", fmt.Errorf("no service %q found in any namespace; %s", l.name, l.notFound)
	default:
		return "", fmt.Errorf("service %q found in multiple namespaces (%s); set %s to pick one",
			l.name, strings.Join(namespaces, ", "), l.envVar)
	}
}

func findNamespace(ctx context.Context, l serviceLookup) (string, error) {
	if ns := os.Getenv(l.envVar); ns != "" {
		return ns, nil
	}
	cmd := exec.CommandContext(ctx, "kubectl", "get", "svc", "-A",
		"--field-selector", "metadata.name="+l.name,
		"-o", "jsonpath={.items[*].metadata.namespace}")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("looking up service %q: %w", l.name, err)
	}
	return pickNamespace(l, string(out))
}

// This is a helper program to run the controller and the source controller port forward in parallel for local development.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Resolved before the goroutines start so a failure prints one message.
	sourceNS, err := findNamespace(ctx, sourceController)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	tagsNS, err := findNamespace(ctx, imageReflectorController)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The controller scopes its CustomResourceDefinitionSource cache to this namespace.
	controllerNS, err := findNamespace(ctx, chrysopoeia)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		fmt.Println("Running port forward to source controller in", sourceNS)
		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", sourceNS, "svc/source-controller", "8091:80")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})
	g.Go(func() error {
		fmt.Println("Running port forward to image reflector controller in", tagsNS)
		cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", tagsNS, "svc/image-reflector-controller-tags", "8090:8090")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})
	g.Go(func() error {
		fmt.Println("Running controller for namespace", controllerNS)
		cmd := exec.CommandContext(ctx, "go", "run", "main.go", "controller",
			"--source-controller-hostname-override=localhost:8091",
			"--image-reflector-controller-hostname=localhost:8090",
		)
		cmd.Env = append(os.Environ(), "POD_NAMESPACE="+controllerNS)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	})

	if err := g.Wait(); err != nil {
		os.Exit(1)
	}
}
