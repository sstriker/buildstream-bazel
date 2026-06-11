cmake_minimum_required(VERSION 3.20)
project(dropaudit C)

# A: file(WRITE) a build-dir SOURCE consumed by a target.
file(WRITE ${CMAKE_BINARY_DIR}/gen_a.c "int a(void){return 1;}\n")

# B: file(COPY) a committed source into the build dir, consumed from there.
file(COPY ${CMAKE_CURRENT_SOURCE_DIR}/b.c DESTINATION ${CMAKE_BINARY_DIR}/copied)

# C: file(TOUCH) an empty placeholder source.
file(TOUCH ${CMAKE_BINARY_DIR}/gen_c.c)

# D: string-built header consumed via build-dir include.
set(_hdr "#define D_VALUE 4\n")
file(WRITE ${CMAKE_BINARY_DIR}/gen_d.h "${_hdr}")

add_executable(app main.c
  ${CMAKE_BINARY_DIR}/gen_a.c
  ${CMAKE_BINARY_DIR}/copied/b.c
  ${CMAKE_BINARY_DIR}/gen_c.c)
target_include_directories(app PRIVATE ${CMAKE_BINARY_DIR})
