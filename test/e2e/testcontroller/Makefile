.PHONY: all
all: generate manifests

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	go tool sigs.k8s.io/controller-tools/cmd/controller-gen crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	go tool sigs.k8s.io/controller-tools/cmd/controller-gen object applyconfiguration paths="./..."
