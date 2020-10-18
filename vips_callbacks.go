package main

/*
#cgo pkg-config: vips
#cgo CFLAGS: -O3
#include "vips.h"

extern long imgproxy_write(VipsTarget*, const void*, long, void*);
extern void imgproxy_finish(VipsTarget*, void*);

VipsTarget* imgproxy_new_writer_target(void* user) {
	VipsTargetCustom *target = vips_target_custom_new();
	g_signal_connect(target, "write", G_CALLBACK(imgproxy_write), user);
	g_signal_connect(target, "finish", G_CALLBACK(imgproxy_finish), user);
	return VIPS_TARGET(target);
}
*/
import "C"
