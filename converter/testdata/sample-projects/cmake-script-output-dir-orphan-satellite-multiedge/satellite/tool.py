#!/usr/bin/env python3
open("fa.c", "w").write('#include "fa.h"\nint fa_value(void){return 7;}\n')
open("fa.h", "w").write("int fa_value(void);\n")
