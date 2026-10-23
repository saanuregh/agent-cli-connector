// The bundled compiler-rt defines roundf128 only as a COFF weak external,
// which GNU ld (MinGW gcc) leaves unresolved, failing the link. This strong
// roundf128 jumps to compiler-rt's implementation. It is a jump, not a C
// call, because gcc's __float128 calling convention on Windows differs from
// the one the Zig code and compiler-rt use. lld resolves either.
__asm__(
    ".text\n"
    ".globl roundf128\n"
    "roundf128:\n"
    "\tjmp \".weak.roundf128.default\"\n");
