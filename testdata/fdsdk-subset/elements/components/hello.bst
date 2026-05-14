kind: cmake

description: |
  hello-world equivalent in the orchestrator fixture: a single cmake
  element with one runtime dep on base. Reuses the converter's
  hello-world sample via a relative path; the orchestrator's
  --sources-base flag lets test code override per-element source
  resolution without depending on this path scheme.

depends:
- base.bst

sources:
- kind: local
  path: ../../../../../converter/testdata/sample-projects/hello-world
