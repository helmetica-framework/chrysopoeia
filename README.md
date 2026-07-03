# chrysopoeia

> chrysopoeia (from Ancient Greek χρυσοποιία (khrusopoiía) 'gold-making')

A controller creating CRDs from Helm charts by translating their `values.yaml`.

## Quickstart

```bash
kubectl apply -k https://github.com/fluxcd/source-controller//config/default

kubectl apply -k config/crd
kubectl apply -k config/samples

make run

kubectl get crds instances.v6.podinfo.bundles.appcat.io
```

## Libraries

* [schemagen](https://pkg.go.dev/github.com/helmetica-framework/chrysopoeia/pkg/schemagen) - Generate CRDs from Helm charts.
