# Helper function declaring a header-only lib (the abseil absl_cc_library shape).
function(add_iface_lib name)
  # inside the helper body
  add_library(${name} INTERFACE)
  target_include_directories(${name} INTERFACE ${CMAKE_CURRENT_SOURCE_DIR}/include)
endfunction()
