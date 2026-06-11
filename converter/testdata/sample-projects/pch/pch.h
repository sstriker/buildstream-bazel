#pragma once
#include <string>
#define PCH_PROVIDED 1
/* Warning-suppression probe: unused const fires -Wunused-const-variable
 * in every including TU when seen DIRECTLY; cmake suppresses it via
 * cmake_pch.hxx's system_header pragma. */
static const int pch_unused_probe = 0;
