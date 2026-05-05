/* sysv.S - x86_64 assembly source for the autotools-asm
 * fixture. Exercises the .S extension (cpp-then-assemble) that
 * the converter must recognize. ASM_VARIANT is supplied via
 * the per-target CFLAGS in Makefile.in; cpp expands it before
 * the assembler sees the .S body.
 */

	.text
	.globl	arch_value
	.type	arch_value, @function
arch_value:
	movl	$ASM_VARIANT, %eax
	ret
	.size	arch_value, .-arch_value

	.section	.note.GNU-stack,"",@progbits
