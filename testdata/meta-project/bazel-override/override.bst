kind: manual

# This element is declared kind:manual — under normal write-a
# dispatch it would render through the trace-driven pipeline.
# The render gate (scripts/meta-bazel-override.sh) flips it to
# kind:bazel out-of-band by passing --build-files-dir at the
# override/ directory next to this .bst, where
# override.BUILD.bazel lives. The asserted behavior: write-a
# treats this element as kind:bazel and emits the operator's
# BUILD.bazel into project B verbatim.

sources:
- kind: local
  path: sources
