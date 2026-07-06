# chrysopoeia

> chrysopoeia (from Ancient Greek χρυσοποιία (khrusopoiía) 'gold-making')

A controller creating CRDs from Helm charts by translating their `values.yaml`.

## Quickstart

```bash
kubectl apply -k config/flux

kubectl apply -k config/crd
kubectl apply -k config/samples

make run

kubectl get -k config/samples
kubectl get crds podinfos.v6.podinfo.helmetica-bundles.io
```

## Libraries

* [schemagen](https://pkg.go.dev/github.com/helmetica-framework/chrysopoeia/pkg/schemagen) - Generate CRDs from Helm charts.
* [breakagedetection](https://pkg.go.dev/github.com/helmetica-framework/chrysopoeia/pkg/breakagedetection) - Detect breaking changes between CRD versions.
