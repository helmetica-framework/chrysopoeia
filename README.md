# chrysopoeia

> chrysopoeia (from Ancient Greek χρυσοποιία (khrusopoiía) 'gold-making')

A controller creating CRDs from Helm charts by translating their `values.yaml`.
Every change to the instances from the created CRDs is preserved in an `InstanceRevision`.

The most recent approved revision is deployed using FluxCDs [Helm controller](https://fluxcd.io/flux/components/helm/).

## Quickstart

```bash
kubectl apply -k config/flux

kubectl apply -k config/crd
kubectl apply -k config/samples

just run

kubectl get -k config/samples
kubectl get crds podinfos.v6.podinfo.helmetica-bundles.io
```

## Libraries

* [schemagen](https://pkg.go.dev/github.com/helmetica-framework/chrysopoeia/pkg/schemagen) - Generate CRDs from Helm charts.
* [breakagedetection](https://pkg.go.dev/github.com/helmetica-framework/chrysopoeia/pkg/breakagedetection) - Detect breaking changes between CRD versions.
* [celvalues](https://pkg.go.dev/github.com/helmetica-framework/chrysopoeia/pkg/celvalues) - Evaluate the CEL expressions in a wrapper chart's values.yaml.

## Operations

### Inject platform values into Helm values

The controller allows injecting values into all managed Helm values under the top-level key `__platform__`.
This can be used to pass platform information such as Kubernetes distribution or load balancer capability.

The values are collected from config maps in the controller namespace and added to the revision.
Every change to one of the config maps creates new revisions for all managed instances.

It is recommended to group values with a unique key and version with stability guarantee similar to Kubernetes APIs.
Values are merged using the upstream Helm value merging.
The annotations must hold a numeric priority. Higher priorities are authoritative over lower priorities when merging. If two annotations hold the same priority the config maps are sorted alphabetically and the later one is authoritative.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: defaults
  annotations:
    chrysopoeia.io/platform-values: "-100"
data:
  values.json: |
    {
      "io.vshn.distribution/v1beta1": {
        "distribution": "unknown"
      }
    }
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: distribution
  annotations:
    chrysopoeia.io/platform-values: "0"
data:
  values.json: |
    {
      "io.vshn.distribution/v1beta1": {
        "distribution": "talos"
      }
    }
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: loadbalancer-capabilities
  annotations:
    chrysopoeia.io/platform-values: "0"
data:
  values.json: |
    {
      "io.vshn.capabilities.loadbalancers/v1alpha1": {
        "networks": ["TCP", "UDP"]
      }
    }
```

would generate the following revision:

```yaml
apiVersion: helmetica.io/v1
kind: InstanceRevision
metadata:
  name: claim-XXXX
spec:
  approvedAt: "2026-08-10T17:02:56Z"
  ociUrl: oci://example.com/chart/example
  values:
    example:
      server:
        ingress: true
    __platform__:
      io.vshn.distribution/v1beta1:
        distribution: talos
      io.vshn.capabilities.loadbalancers/v1alpha1:
        networks: [TCP, UDP]
  version: 1.4.5@sha256:XXXX
```
