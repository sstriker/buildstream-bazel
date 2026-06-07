# cmake -P integration runner: invoke the built CLI on the argument and
# assert it echoes it back. The idiomatic Bazel form is an sh_test/diff_test
# over //:cli — there is no faithful mechanical translation, which is exactly
# why this surfaces as an `actionable` conversion-todo.
if(NOT DEFINED CLI)
  message(FATAL_ERROR "CLI not set")
endif()
set(input "${CMAKE_ARGV3}")
execute_process(COMMAND "${CLI}" "${input}"
                OUTPUT_VARIABLE out
                OUTPUT_STRIP_TRAILING_WHITESPACE
                RESULT_VARIABLE rc)
if(NOT rc EQUAL 0)
  message(FATAL_ERROR "cli exited ${rc}")
endif()
if(NOT out STREQUAL input)
  message(FATAL_ERROR "expected '${input}', got '${out}'")
endif()
