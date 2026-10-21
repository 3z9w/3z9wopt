#include "textflag.h"

// func cpuidAsm(ax, cx uint32, out *[4]uint32)
//   AX = leaf/subleaf 入参
//   out[0]=EAX out[1]=EBX out[2]=ECX out[3]=EDX
TEXT ·cpuidAsm(SB), NOSPLIT, $0-16
	MOVQ out+8(FP), R11
	MOVL ax+0(FP), AX
	MOVL cx+4(FP), CX
	CPUID
	MOVL AX, 0(R11)
	MOVL BX, 4(R11)
	MOVL CX, 8(R11)
	MOVL DX, 12(R11)
	RET
