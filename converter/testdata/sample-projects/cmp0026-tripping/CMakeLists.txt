cmake_minimum_required(VERSION 3.20)
project(cmp0026_tripping LANGUAGES C VERSION 0.1.0)

# The pre-3.0 idiom yasm (and other vintage packages) use to
# resolve an executable target to its on-disk path:
# get_target_property(<var> <tgt> LOCATION). cmake 3.x emits a
# CMP0026 deprecation warning but still resolves; cmake 4.x
# removed the OLD behaviour and fatal-errors at configure with
#   "The LOCATION property may not be read from target ..."
#
# This fixture deliberately trips that fatal-error under cmake
# 4.x so the integration test in
# converter/internal/cmakerun/cmp0026_e2e_test.go can verify
# convert-element-cmake's stderr-scrape annotates the failure
# with the patch_cmds workaround hint. Under cmake 3.x the
# configure succeeds (with a deprecation warning); the
# integration test detects that case and skips.
add_executable(emitter emitter.c)

# The offending call. cmake 3.x: warning + sets _emitter_loc to
# the resolved path. cmake 4.x: fatal error before the property
# read returns.
get_target_property(_emitter_loc emitter LOCATION)

# Reference the captured location so it's not dead code (and so
# cmake's policy enforcement actually fires on the read).
add_custom_target(emit_payload
    COMMAND ${_emitter_loc}
    DEPENDS emitter)
