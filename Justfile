set shell := ["bash", "-euo", "pipefail", "-c"]

export GOEXPERIMENT := "jsonv2"

img_tag := env("IMG_TAG", "latest")
bin_filename := env("BIN_FILENAME", "chrysopoeia")
localbin := justfile_directory() / "bin"

kustomize := "go tool sigs.k8s.io/kustomize/kustomize/v5"
kind := "go tool sigs.k8s.io/kind"

# Image URL to use all building/pushing image targets
ghcr_img := env("GHCR_IMG", "ghcr.io/helmetica-framework/chrysopoeia:" + img_tag)

envtest_k8s_version := env("ENVTEST_K8S_VERSION", "1.36.2")

# Show this help
default:
    @just --list --unsorted

# Invokes the build recipe
all: build

# Run tests
test: manifests generate
    mkdir -p {{ localbin }}
    # -race requires cgo, so don't inherit a CGO_ENABLED=0 from the environment.
    CGO_ENABLED=1 \
    KUBEBUILDER_ASSETS="$(go tool sigs.k8s.io/controller-runtime/tools/setup-envtest use {{ envtest_k8s_version }} --bin-dir {{ localbin }} -p path)" \
        go test ./... -race -coverprofile cover.tmp.out
    grep -v "zz_generated.deepcopy.go" cover.tmp.out > cover.out

# Build manager binary
build: generate manifests fmt vet binary

# Build the binary without running generators
binary:
    @echo "GOOS=$(go env GOOS) GOARCH=$(go env GOARCH)"
    CGO_ENABLED=0 go build -o {{ bin_filename }}

# Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
manifests:
    go tool sigs.k8s.io/controller-tools/cmd/controller-gen rbac:roleName=manager-role crd:generateEmbeddedObjectMeta=true applyconfiguration webhook paths="./..." output:crd:artifacts:config=config/crd/bases

# Generate documentation
docs:
    @echo "Nothing to do yet"

# Generate manifests e.g. CRD, RBAC etc.
generate:
    go generate ./...
    go tool sigs.k8s.io/controller-tools/cmd/controller-gen object paths="./..."

# Run go fmt against code
fmt:
    go fmt ./...

# Run go vet against code
vet:
    go vet ./...

# All-in-one linting
lint: fmt vet generate manifests docs
    @echo 'Checking kustomize build ...'
    {{ kustomize }} build config/crd -o /dev/null
    {{ kustomize }} build config/default -o /dev/null
    @echo 'Check for uncommitted changes ...'
    git diff --exit-code

# Build the docker image
build-docker: binary
    docker build . --tag {{ ghcr_img }}

# Cleans up the generated resources
clean:
    rm -rf contrib/completion dist/ cover.out {{ bin_filename }} ||:

# Run a controller from your host.
run: manifests generate fmt vet
    go run ./hack/makerun
