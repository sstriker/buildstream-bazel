#!/usr/bin/env python3
open("foo.c","w").write('#include "foo.h"\nint foo_value(void){return 7;}\n')
open("foo.h","w").write("int foo_value(void);\n")
