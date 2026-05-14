kind: cmake

description: |
  Cross-element consumer that depends on hello via find_package. Used
  by the orchestrator's synth-prefix and imports-manifest tests
  (M3a steps 3 and 4).

depends:
- base.bst
- filename: components/hello.bst
  type: build

sources:
- kind: local
  path: ../../files/uses-hello
