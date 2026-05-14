kind: manual

description: |
  Stub base element for the orchestrator fixture. The FDSDK has a deeper
  base layer; we collapse it to a single manual stub since the
  orchestrator only cares about kind:cmake elements as conversion
  targets.

sources:
- kind: local
  path: files/base
