package main

/*
#cgo pkg-config: vips
#cgo LDFLAGS: -s -w
#cgo CFLAGS: -O3
#include "vips.h"
*/
import "C"
import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"unsafe"

	"github.com/mattn/go-pointer"
)

type vipsImage struct {
	VipsImage *C.VipsImage
}

var (
	vipsSupportSmartcrop bool
	vipsTypeSupportLoad  = make(map[imageType]bool)
	vipsTypeSupportSave  = make(map[imageType]bool)

	watermark *imageData
)

var vipsConf struct {
	JpegProgressive       C.int
	PngInterlaced         C.int
	PngQuantize           C.int
	PngQuantizationColors C.int
	WatermarkOpacity      C.double
}

const (
	vipsAngleD0   = C.VIPS_ANGLE_D0
	vipsAngleD90  = C.VIPS_ANGLE_D90
	vipsAngleD180 = C.VIPS_ANGLE_D180
	vipsAngleD270 = C.VIPS_ANGLE_D270
)

func initVips() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := C.vips_initialize(); err != 0 {
		C.vips_shutdown()
		return fmt.Errorf("unable to start vips!")
	}

	// Disable libvips cache. Since processing pipeline is fine tuned, we won't get much profit from it.
	// Enabled cache can cause SIGSEGV on Musl-based systems like Alpine.
	C.vips_cache_set_max_mem(0)
	C.vips_cache_set_max(0)

	C.vips_concurrency_set(1)

	// Vector calculations cause SIGSEGV sometimes when working with JPEG.
	// It's better to disable it since profit it quite small
	C.vips_vector_set_enabled(0)

	if len(os.Getenv("IMGPROXY_VIPS_LEAK_CHECK")) > 0 {
		C.vips_leak_set(C.gboolean(1))
	}

	if len(os.Getenv("IMGPROXY_VIPS_CACHE_TRACE")) > 0 {
		C.vips_cache_set_trace(C.gboolean(1))
	}

	vipsSupportSmartcrop = C.vips_support_smartcrop() == 1

	for _, imgtype := range imageTypes {
		vipsTypeSupportLoad[imgtype] = int(C.vips_type_find_load_go(C.int(imgtype))) != 0
		vipsTypeSupportSave[imgtype] = int(C.vips_type_find_save_go(C.int(imgtype))) != 0
	}

	if conf.JpegProgressive {
		vipsConf.JpegProgressive = C.int(1)
	}

	if conf.PngInterlaced {
		vipsConf.PngInterlaced = C.int(1)
	}

	if conf.PngQuantize {
		vipsConf.PngQuantize = C.int(1)
	}

	vipsConf.PngQuantizationColors = C.int(conf.PngQuantizationColors)

	vipsConf.WatermarkOpacity = C.double(conf.WatermarkOpacity)

	if err := vipsLoadWatermark(); err != nil {
		C.vips_shutdown()
		return fmt.Errorf("Can't load watermark: %s", err)
	}

	return nil
}

func shutdownVips() {
	C.vips_shutdown()
}

func vipsGetMem() float64 {
	return float64(C.vips_tracked_get_mem())
}

func vipsGetMemHighwater() float64 {
	return float64(C.vips_tracked_get_mem_highwater())
}

func vipsGetAllocs() float64 {
	return float64(C.vips_tracked_get_allocs())
}

func vipsCleanup() {
	C.vips_cleanup()
}

func vipsError() error {
	return newUnexpectedError(C.GoString(C.vips_error_buffer()), 1)
}

func vipsLoadWatermark() (err error) {
	watermark, err = getWatermarkData()
	return
}

func gbool(b bool) C.gboolean {
	if b {
		return C.gboolean(1)
	}
	return C.gboolean(0)
}

func (img *vipsImage) Width() int {
	return int(img.VipsImage.Xsize)
}

func (img *vipsImage) Height() int {
	return int(img.VipsImage.Ysize)
}

func (img *vipsImage) Load(data []byte, imgtype imageType, shrink int, scale float64, pages int) error {
	var tmp *C.VipsImage

	err := C.int(0)

	switch imgtype {
	case imageTypeJPEG:
		err = C.vips_jpegload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), C.int(shrink), &tmp)
	case imageTypePNG:
		err = C.vips_pngload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), &tmp)
	case imageTypeWEBP:
		err = C.vips_webpload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), C.double(scale), C.int(pages), &tmp)
	case imageTypeGIF:
		err = C.vips_gifload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), C.int(pages), &tmp)
	case imageTypeSVG:
		err = C.vips_svgload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), C.double(scale), &tmp)
	case imageTypeHEIC, imageTypeAVIF:
		err = C.vips_heifload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), &tmp)
	case imageTypeBMP:
		err = C.vips_bmpload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), &tmp)
	case imageTypeTIFF:
		err = C.vips_tiffload_go(unsafe.Pointer(&data[0]), C.size_t(len(data)), &tmp)
	}
	if err != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)

	return nil
}

func (img *vipsImage) Save(w io.Writer, imgtype imageType, quality int, stripMeta bool) (context.CancelFunc, error) {
	if imgtype == imageTypeICO {
		return func() {}, img.SaveAsIco(w)
	}

	cancel := func() {
		// don't think we actually need this
	}

	wp := pointer.Save(w)
	defer pointer.Unref(wp)

	target := C.imgproxy_new_writer_target(wp)
	defer C.g_object_unref(C.gpointer(target))
	err := C.int(0)

	switch imgtype {
	case imageTypeJPEG:
		err = C.vips_jpegsave_go(img.VipsImage, target, C.int(quality), vipsConf.JpegProgressive, gbool(stripMeta))
	case imageTypePNG:
		err = C.vips_pngsave_go(img.VipsImage, target, vipsConf.PngInterlaced, vipsConf.PngQuantize, vipsConf.PngQuantizationColors)
	case imageTypeWEBP:
		err = C.vips_webpsave_go(img.VipsImage, target, C.int(quality), gbool(stripMeta))
	case imageTypeGIF:
		err = C.vips_gifsave_go(img.VipsImage, target)
	case imageTypeAVIF:
		err = C.vips_avifsave_go(img.VipsImage, target, C.int(quality))
	case imageTypeBMP:
		err = C.vips_bmpsave_go(img.VipsImage, target)
	case imageTypeTIFF:
		err = C.vips_tiffsave_go(img.VipsImage, target, C.int(quality))
	}
	if err != 0 {
		return cancel, vipsError()
	}

	return cancel, nil
}

func (img *vipsImage) SaveAsIco(w io.Writer) error {
	if img.Width() > 256 || img.Height() > 256 {
		return errors.New("Image dimensions is too big. Max dimension size for ICO is 256")
	}

	var imgData bytes.Buffer
	wp := pointer.Save(imgData)
	defer pointer.Unref(wp)

	target := C.imgproxy_new_writer_target(wp)
	defer C.g_object_unref(C.gpointer(target))

	if C.vips_pngsave_go(img.VipsImage, target, 0, 0, 256) != 0 {
		return vipsError()
	}

	// ICONDIR header
	if _, err := w.Write([]byte{0, 0, 1, 0, 1, 0}); err != nil {
		return err
	}

	// ICONDIRENTRY
	if _, err := w.Write([]byte{
		byte(img.Width() % 256),
		byte(img.Height() % 256),
	}); err != nil {
		return err
	}
	// Number of colors. Not supported in our case
	if _, err := w.Write([]byte{0}); err != nil {
		return err
	}
	// Reserved
	if _, err := w.Write([]byte{0}); err != nil {
		return err
	}
	// Color planes. Always 1 in our case
	if _, err := w.Write([]byte{1, 0}); err != nil {
		return err
	}
	// Bits per pixel
	if img.HasAlpha() {
		if _, err := w.Write([]byte{32, 0}); err != nil {
			return err
		}
	} else {
		if _, err := w.Write([]byte{24, 0}); err != nil {
			return err
		}
	}
	// Image data size
	if err := binary.Write(w, binary.LittleEndian, uint32(imgData.Len())); err != nil {
		return err
	}
	// Image data offset. Always 22 in our case
	if _, err := w.Write([]byte{22, 0, 0, 0}); err != nil {
		return err
	}

	if _, err := w.Write(imgData.Bytes()); err != nil {
		return err
	}

	return nil
}

func (img *vipsImage) Clear() {
	if img.VipsImage != nil {
		C.clear_image(&img.VipsImage)
	}
}

func (img *vipsImage) Arrayjoin(in []*vipsImage) error {
	var tmp *C.VipsImage

	arr := make([]*C.VipsImage, len(in))
	for i, im := range in {
		arr[i] = im.VipsImage
	}

	if C.vips_arrayjoin_go(&arr[0], &tmp, C.int(len(arr))) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func vipsSupportAnimation(imgtype imageType) bool {
	return imgtype == imageTypeGIF ||
		(imgtype == imageTypeWEBP && C.vips_support_webp_animation() != 0)
}

func (img *vipsImage) IsAnimated() bool {
	return C.vips_is_animated(img.VipsImage) > 0
}

func (img *vipsImage) HasAlpha() bool {
	return C.vips_image_hasalpha_go(img.VipsImage) > 0
}

func (img *vipsImage) GetInt(name string) (int, error) {
	var i C.int

	if C.vips_image_get_int(img.VipsImage, cachedCString(name), &i) != 0 {
		return 0, vipsError()
	}
	return int(i), nil
}

func (img *vipsImage) SetInt(name string, value int) {
	C.vips_image_set_int(img.VipsImage, cachedCString(name), C.int(value))
}

func (img *vipsImage) CastUchar() error {
	var tmp *C.VipsImage

	if C.vips_image_get_format(img.VipsImage) != C.VIPS_FORMAT_UCHAR {
		if C.vips_cast_go(img.VipsImage, &tmp, C.VIPS_FORMAT_UCHAR) != 0 {
			return vipsError()
		}
		C.swap_and_clear(&img.VipsImage, tmp)
	}

	return nil
}

func (img *vipsImage) Rad2Float() error {
	var tmp *C.VipsImage

	if C.vips_image_get_coding(img.VipsImage) == C.VIPS_CODING_RAD {
		if C.vips_rad2float_go(img.VipsImage, &tmp) != 0 {
			return vipsError()
		}
		C.swap_and_clear(&img.VipsImage, tmp)
	}

	return nil
}

func (img *vipsImage) Resize(scale float64, hasAlpa bool) error {
	var tmp *C.VipsImage

	if hasAlpa {
		if C.vips_resize_with_premultiply(img.VipsImage, &tmp, C.double(scale)) != 0 {
			return vipsError()
		}
	} else {
		if C.vips_resize_go(img.VipsImage, &tmp, C.double(scale)) != 0 {
			return vipsError()
		}
	}

	C.swap_and_clear(&img.VipsImage, tmp)

	return nil
}

func (img *vipsImage) Orientation() C.int {
	return C.vips_get_orientation(img.VipsImage)
}

func (img *vipsImage) Rotate(angle int) error {
	var tmp *C.VipsImage

	if C.vips_rot_go(img.VipsImage, &tmp, C.VipsAngle(angle)) != 0 {
		return vipsError()
	}

	C.vips_autorot_remove_angle(tmp)

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Flip() error {
	var tmp *C.VipsImage

	if C.vips_flip_horizontal_go(img.VipsImage, &tmp) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Crop(left, top, width, height int) error {
	var tmp *C.VipsImage

	if C.vips_extract_area_go(img.VipsImage, &tmp, C.int(left), C.int(top), C.int(width), C.int(height)) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Extract(out *vipsImage, left, top, width, height int) error {
	if C.vips_extract_area_go(img.VipsImage, &out.VipsImage, C.int(left), C.int(top), C.int(width), C.int(height)) != 0 {
		return vipsError()
	}
	return nil
}

func (img *vipsImage) SmartCrop(width, height int) error {
	var tmp *C.VipsImage

	if C.vips_smartcrop_go(img.VipsImage, &tmp, C.int(width), C.int(height)) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Trim(threshold float64, smart bool, color rgbColor, equalHor bool, equalVer bool) error {
	var tmp *C.VipsImage

	if err := img.CopyMemory(); err != nil {
		return err
	}

	if C.vips_trim(img.VipsImage, &tmp, C.double(threshold),
		gbool(smart), C.double(color.R), C.double(color.G), C.double(color.B),
		gbool(equalHor), gbool(equalVer)) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) EnsureAlpha() error {
	var tmp *C.VipsImage

	if C.vips_ensure_alpha(img.VipsImage, &tmp) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Flatten(bg rgbColor) error {
	var tmp *C.VipsImage

	if C.vips_flatten_go(img.VipsImage, &tmp, C.double(bg.R), C.double(bg.G), C.double(bg.B)) != 0 {
		return vipsError()
	}
	C.swap_and_clear(&img.VipsImage, tmp)

	return nil
}

func (img *vipsImage) Blur(sigma float32) error {
	var tmp *C.VipsImage

	if C.vips_gaussblur_go(img.VipsImage, &tmp, C.double(sigma)) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Sharpen(sigma float32) error {
	var tmp *C.VipsImage

	if C.vips_sharpen_go(img.VipsImage, &tmp, C.double(sigma)) != 0 {
		return vipsError()
	}

	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) ImportColourProfile(evenSRGB bool) error {
	var tmp *C.VipsImage

	if img.VipsImage.Coding != C.VIPS_CODING_NONE {
		return nil
	}

	if img.VipsImage.BandFmt != C.VIPS_FORMAT_UCHAR && img.VipsImage.BandFmt != C.VIPS_FORMAT_USHORT {
		return nil
	}

	profile := (*C.char)(nil)

	if C.vips_has_embedded_icc(img.VipsImage) == 0 {
		// No embedded profile
		// If vips doesn't have built-in profile, use profile built-in to imgproxy for CMYK
		// TODO: Remove this. Supporting built-in profiles is pain, vips does it better
		if img.VipsImage.Type == C.VIPS_INTERPRETATION_CMYK && C.vips_support_builtin_icc() == 0 {
			p, err := cmykProfilePath()
			if err != nil {
				return err
			}
			profile = cachedCString(p)
		} else {
			// imgproxy doesn't have built-in profile for other interpretations,
			// so we can't do anything here
			return nil
		}
	}

	// Don't import sRGB IEC61966 2.1 unless evenSRGB
	if img.VipsImage.Type == C.VIPS_INTERPRETATION_sRGB && !evenSRGB && C.vips_icc_is_srgb_iec61966(img.VipsImage) != 0 {
		return nil
	}

	if C.vips_icc_import_go(img.VipsImage, &tmp, profile) == 0 {
		C.swap_and_clear(&img.VipsImage, tmp)
	} else {
		logWarning("Can't import ICC profile: %s", vipsError())
	}

	return nil
}

func (img *vipsImage) IsSRGB() bool {
	return img.VipsImage.Type == C.VIPS_INTERPRETATION_sRGB
}

func (img *vipsImage) LinearColourspace() error {
	return img.Colorspace(C.VIPS_INTERPRETATION_scRGB)
}

func (img *vipsImage) RgbColourspace() error {
	return img.Colorspace(C.VIPS_INTERPRETATION_sRGB)
}

func (img *vipsImage) Colorspace(colorspace C.VipsInterpretation) error {
	if img.VipsImage.Type != colorspace {
		var tmp *C.VipsImage

		if C.vips_colourspace_go(img.VipsImage, &tmp, colorspace) != 0 {
			return vipsError()
		}
		C.swap_and_clear(&img.VipsImage, tmp)
	}

	return nil
}

func (img *vipsImage) CopyMemory() error {
	var tmp *C.VipsImage
	if tmp = C.vips_image_copy_memory(img.VipsImage); tmp == nil {
		return vipsError()
	}
	C.swap_and_clear(&img.VipsImage, tmp)
	return nil
}

func (img *vipsImage) Replicate(width, height int) error {
	var tmp *C.VipsImage

	if C.vips_replicate_go(img.VipsImage, &tmp, C.int(width), C.int(height)) != 0 {
		return vipsError()
	}
	C.swap_and_clear(&img.VipsImage, tmp)

	return nil
}

func (img *vipsImage) Embed(width, height int, offX, offY int, bg rgbColor, transpBg bool) error {
	var tmp *C.VipsImage

	if err := img.RgbColourspace(); err != nil {
		return err
	}

	var bgc []C.double
	if transpBg {
		if !img.HasAlpha() {
			if C.vips_addalpha_go(img.VipsImage, &tmp) != 0 {
				return vipsError()
			}
			C.swap_and_clear(&img.VipsImage, tmp)
		}

		bgc = []C.double{C.double(0)}
	} else {
		bgc = []C.double{C.double(bg.R), C.double(bg.G), C.double(bg.B), 1.0}
	}

	bgn := minInt(int(img.VipsImage.Bands), len(bgc))

	if C.vips_embed_go(img.VipsImage, &tmp, C.int(offX), C.int(offY), C.int(width), C.int(height), &bgc[0], C.int(bgn)) != 0 {
		return vipsError()
	}
	C.swap_and_clear(&img.VipsImage, tmp)

	return nil
}

func (img *vipsImage) ApplyWatermark(wm *vipsImage, opacity float64) error {
	var tmp *C.VipsImage

	if C.vips_apply_watermark(img.VipsImage, wm.VipsImage, &tmp, C.double(opacity)) != 0 {
		return vipsError()
	}
	C.swap_and_clear(&img.VipsImage, tmp)

	return nil
}

//export imgproxy_write
func imgproxy_write(target *C.VipsTargetCustom, buffer unsafe.Pointer, length C.long, user unsafe.Pointer) C.long {
	v := pointer.Restore(user).(io.Writer)
	d := C.GoBytes(buffer, C.int(length))
	n, err := v.Write(d)
	if err != nil {
		return -1
	}
	return C.long(n)
}

//export imgproxy_finish
func imgproxy_finish(target *C.VipsTargetCustom, user unsafe.Pointer) {
	u := (interface{})(user)
	if flusher, ok := u.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	if closer, ok := u.(io.Closer); ok {
		closer.Close()
	}
}
