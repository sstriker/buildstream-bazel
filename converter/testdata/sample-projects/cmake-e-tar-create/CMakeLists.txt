cmake_minimum_required(VERSION 3.20)
project(tarcreate C)

# Package two committed source files into a gzip tarball at configure
# time — the cmake -E tar CREATE idiom. The faithful Bazel shape is
# pkg_tar (rules_pkg), emitted via the codegen-recognizer registry's
# generic native-rule substrate.
execute_process(
  COMMAND ${CMAKE_COMMAND} -E tar czf ${CMAKE_BINARY_DIR}/bundle.tar.gz
          ${CMAKE_CURRENT_SOURCE_DIR}/a.txt
          ${CMAKE_CURRENT_SOURCE_DIR}/b.txt)
