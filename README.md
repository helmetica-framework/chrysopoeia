# chrysopoeia

> chrysopoeia (from Ancient Greek χρυσοποιία (khrusopoiía) 'gold-making')

A controller creating CRDs from Helm charts by translating their `values.yaml`, recording every change to an instance in a `ClaimRevision` and deploying the most recent approved revision with Flux's Helm controller.

Documentation: https://docs.helmetica.io/chrysopoeia/

## Development

```bash
just test
just run    # against the cluster in your KUBECONFIG
just docs   # preview the docs
```
