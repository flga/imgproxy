package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	responseGzipBufPool *bufPool
	responseGzipPool    *gzipPool

	processingSem chan struct{}

	headerVaryValue string
	fallbackImage   *imageData
)

func initProcessingHandler() error {
	var err error

	processingSem = make(chan struct{}, conf.Concurrency)

	if conf.GZipCompression > 0 {
		responseGzipBufPool = newBufPool("gzip", conf.Concurrency, conf.GZipBufferSize)
		if responseGzipPool, err = newGzipPool(conf.Concurrency); err != nil {
			return err
		}
	}

	vary := make([]string, 0)

	if conf.EnableWebpDetection || conf.EnforceWebp {
		vary = append(vary, "Accept")
	}

	if conf.GZipCompression > 0 {
		vary = append(vary, "Accept-Encoding")
	}

	if conf.EnableClientHints {
		vary = append(vary, "DPR", "Viewport-Width", "Width")
	}

	headerVaryValue = strings.Join(vary, ", ")

	if fallbackImage, err = getFallbackImageData(); err != nil {
		return err
	}

	return nil
}

func prerespondWithImage(ctx context.Context, reqID string, imageURL, cacheControl, expires string, po *processingOptions, r *http.Request, rw http.ResponseWriter) (w io.Writer, flush context.CancelFunc) {

	var contentDisposition string
	if len(po.Filename) > 0 {
		contentDisposition = po.Format.ContentDisposition(po.Filename)
	} else {
		contentDisposition = po.Format.ContentDispositionFromURL(imageURL)
	}

	rw.Header().Set("Content-Type", po.Format.Mime())
	rw.Header().Set("Content-Disposition", contentDisposition)

	if !conf.CacheControlPassthrough {
		cacheControl = ""
		expires = ""
	}

	if len(cacheControl) == 0 && len(expires) == 0 {
		cacheControl = fmt.Sprintf("max-age=%d, public", conf.TTL)
		expires = time.Now().Add(time.Second * time.Duration(conf.TTL)).Format(http.TimeFormat)
	}

	if len(cacheControl) > 0 {
		rw.Header().Set("Cache-Control", cacheControl)
	}
	if len(expires) > 0 {
		rw.Header().Set("Expires", expires)
	}

	if len(headerVaryValue) > 0 {
		rw.Header().Set("Vary", headerVaryValue)
	}

	logResponse(reqID, r, 200, nil, &imageURL, po)

	if conf.GZipCompression > 0 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		buf := responseGzipBufPool.Get(0)
		defer responseGzipBufPool.Put(buf)

		gz := responseGzipPool.Get(buf)
		gz.Reset(rw)
		rw.Header().Set("Content-Encoding", "gzip")
		return gz, func() {
			gz.Close()
			responseGzipPool.Put(gz)
		}
	}

	return rw, func() {}
}

func respondWithNotModified(ctx context.Context, reqID string, imageURL string, po *processingOptions, r *http.Request, rw http.ResponseWriter) {
	rw.WriteHeader(304)
	logResponse(reqID, r, 304, nil, &imageURL, po)
}

func handleProcessing(reqID string, rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if newRelicEnabled {
		var newRelicCancel context.CancelFunc
		ctx, newRelicCancel = startNewRelicTransaction(ctx, rw, r)
		defer newRelicCancel()
	}

	if prometheusEnabled {
		prometheusRequestsTotal.Inc()
		defer startPrometheusDuration(prometheusRequestDuration)()
	}

	select {
	case processingSem <- struct{}{}:
	case <-ctx.Done():
		panic(newError(499, "Request was cancelled before processing", "Cancelled"))
	}
	defer func() { <-processingSem }()

	ctx, timeoutCancel := context.WithTimeout(ctx, time.Duration(conf.WriteTimeout)*time.Second)
	defer timeoutCancel()

	imgURL, po, err := parsePath(ctx, r)
	if err != nil {
		panic(err)
	}

	imgdata, cacheControl, expires, downloadcancel, err := downloadImage(ctx, imgURL)
	defer downloadcancel()
	if err != nil {
		if newRelicEnabled {
			sendErrorToNewRelic(ctx, err)
		}
		if prometheusEnabled {
			incrementPrometheusErrorsTotal("download")
		}

		if fallbackImage == nil {
			panic(err)
		}

		if ierr, ok := err.(*imgproxyError); !ok || ierr.Unexpected {
			reportError(err, r)
		}

		logWarning("Could not load image. Using fallback image: %s", err.Error())
		imgdata = fallbackImage
	}

	checkTimeout(ctx)

	if conf.ETagEnabled {
		eTag := calcETag(imgdata, po)
		rw.Header().Set("ETag", eTag)

		if eTag == r.Header.Get("If-None-Match") {
			respondWithNotModified(ctx, reqID, imgURL, po, r, rw)
			return
		}
	}

	checkTimeout(ctx)

	if len(conf.SkipProcessingFormats) > 0 {
		if imgdata.Type == po.Format || po.Format == imageTypeUnknown {
			for _, f := range conf.SkipProcessingFormats {
				if f == imgdata.Type {
					po.Format = imgdata.Type
					w, done := prerespondWithImage(ctx, reqID, imgURL, cacheControl, expires, po, r, rw)
					defer done()
					w.Write(imgdata.Data)
					return
				}
			}
		}
	}

	if po.Format == imageTypeUnknown {
		switch {
		case po.PreferWebP && imageTypeSaveSupport(imageTypeWEBP):
			po.Format = imageTypeWEBP
		case imageTypeSaveSupport(imgdata.Type) && imageTypeGoodForWeb(imgdata.Type):
			po.Format = imgdata.Type
		default:
			po.Format = imageTypeJPEG
		}
	} else if po.EnforceWebP && imageTypeSaveSupport(imageTypeWEBP) {
		po.Format = imageTypeWEBP
	}

	w, done := prerespondWithImage(ctx, reqID, imgURL, cacheControl, expires, po, r, rw)
	defer done()

	processcancel, err := processImage(ctx, w, po, imgdata)
	defer processcancel()
	if err != nil {
		if newRelicEnabled {
			sendErrorToNewRelic(ctx, err)
		}
		if prometheusEnabled {
			incrementPrometheusErrorsTotal("processing")
		}
		panic(err)
	}

	checkTimeout(ctx)

}
