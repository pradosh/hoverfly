package hoverfly

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/SpectoLabs/hoverfly/authentication/backends"
	"github.com/SpectoLabs/hoverfly/cache"
	"github.com/SpectoLabs/hoverfly/metrics"
	"github.com/rusenask/goproxy"
)

// SimulateMode - default mode when Hoverfly looks for captured requests to respond
const SimulateMode = "simulate"

// SynthesizeMode - all requests are sent to middleware to create response
const SynthesizeMode = "synthesize"

// ModifyMode - middleware is applied to outgoing and incoming traffic
const ModifyMode = "modify"

// CaptureMode - requests are captured and stored in cache
const CaptureMode = "capture"

// orPanic - wrapper for logging errors
func orPanic(err error) {
	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Panic("Got error.")
	}
}

// GetNewHoverfly returns a configured ProxyHttpServer and DBClient
func GetNewHoverfly(cfg *Configuration, requestCache, metadataCache cache.Cache, authentication backends.Authentication) *Hoverfly {
	h := &Hoverfly{
		RequestCache:   requestCache,
		MetadataCache:  metadataCache,
		Authentication: authentication,
		HTTP: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.TLSVerification},
		}},
		Cfg:     cfg,
		Counter: metrics.NewModeCounter([]string{SimulateMode, SynthesizeMode, ModifyMode, CaptureMode}),
		Hooks:   make(ActionTypeHooks),
	}
	h.UpdateProxy()
	return h
}

// UpdateProxy - applies hooks
func (d *Hoverfly) UpdateProxy() {
	// creating proxy
	proxy := goproxy.NewProxyHttpServer()

	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile(d.Cfg.Destination))).
		HandleConnect(goproxy.AlwaysMitm)

	// enable curl -p for all hosts on port 80
	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile(d.Cfg.Destination))).
		HijackConnect(func(req *http.Request, client net.Conn, ctx *goproxy.ProxyCtx) {
			defer func() {
				if e := recover(); e != nil {
					ctx.Logf("error connecting to remote: %v", e)
					client.Write([]byte("HTTP/1.1 500 Cannot reach destination\r\n\r\n"))
				}
				client.Close()
			}()
			clientBuf := bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client))
			remote, err := net.Dial("tcp", req.URL.Host)
			orPanic(err)
			remoteBuf := bufio.NewReadWriter(bufio.NewReader(remote), bufio.NewWriter(remote))
			for {
				req, err := http.ReadRequest(clientBuf.Reader)
				orPanic(err)
				orPanic(req.Write(remoteBuf))
				orPanic(remoteBuf.Flush())
				resp, err := http.ReadResponse(remoteBuf.Reader, req)

				orPanic(err)
				orPanic(resp.Write(clientBuf.Writer))
				orPanic(clientBuf.Flush())
			}
		})

	// processing connections
	proxy.OnRequest(goproxy.ReqHostMatches(regexp.MustCompile(d.Cfg.Destination))).DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			req, resp := d.processRequest(r)
			return req, resp
		})

	if d.Cfg.Verbose {
		proxy.OnRequest().DoFunc(
			func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
				log.WithFields(log.Fields{
					"destination": r.Host,
					"path":        r.URL.Path,
					"query":       r.URL.RawQuery,
					"method":      r.Method,
					"mode":        d.Cfg.GetMode(),
				}).Debug("got request..")
				return r, nil
			})
	}

	// intercepts response
	proxy.OnResponse(goproxy.ReqHostMatches(regexp.MustCompile(d.Cfg.Destination))).DoFunc(
		func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
			d.Counter.Count(d.Cfg.GetMode())
			return resp
		})

	proxy.Verbose = d.Cfg.Verbose
	// proxy starting message
	log.WithFields(log.Fields{
		"Destination": d.Cfg.Destination,
		"ProxyPort":   d.Cfg.ProxyPort,
		"Mode":        d.Cfg.GetMode(),
	}).Info("Proxy prepared...")

	d.Proxy = proxy
	return
}

func hoverflyError(req *http.Request, err error, msg string, statusCode int) *http.Response {
	return goproxy.NewResponse(req,
		goproxy.ContentTypeText, statusCode,
		fmt.Sprintf("Hoverfly Error! %s. Got error: %s \n", msg, err.Error()))
}

// processRequest - processes incoming requests and based on proxy state (record/playback)
// returns HTTP response.
func (d *Hoverfly) processRequest(req *http.Request) (*http.Request, *http.Response) {

	mode := d.Cfg.GetMode()

	if mode == CaptureMode {
		newResponse, err := d.captureRequest(req)

		if err != nil {
			return req, hoverflyError(req, err, "Could not capture request", http.StatusServiceUnavailable)
		}
		log.WithFields(log.Fields{
			"mode":        mode,
			"middleware":  d.Cfg.Middleware,
			"path":        req.URL.Path,
			"rawQuery":    req.URL.RawQuery,
			"method":      req.Method,
			"destination": req.Host,
		}).Info("request and response captured")

		return req, newResponse

	} else if mode == SynthesizeMode {
		response, err := SynthesizeResponse(req, d.Cfg.Middleware)

		if err != nil {
			return req, hoverflyError(req, err, "Could not create synthetic response!", http.StatusServiceUnavailable)
		}

		log.WithFields(log.Fields{
			"mode":        mode,
			"middleware":  d.Cfg.Middleware,
			"path":        req.URL.Path,
			"rawQuery":    req.URL.RawQuery,
			"method":      req.Method,
			"destination": req.Host,
		}).Info("synthetic response created successfuly")

		return req, response

	} else if mode == ModifyMode {

		response, err := d.modifyRequestResponse(req, d.Cfg.Middleware)

		if err != nil {
			log.WithFields(log.Fields{
				"error":      err.Error(),
				"middleware": d.Cfg.Middleware,
			}).Error("Got error when performing request modification")
			return req, hoverflyError(
				req,
				err,
				fmt.Sprintf("Middleware (%s) failed or something else happened!", d.Cfg.Middleware),
				http.StatusServiceUnavailable)
		}
		// returning modified response
		return req, response
	}

	newResponse := d.getResponse(req)

	// introduce response delay
	if d.Cfg.ResponseDelay > 0 {

		log.WithFields(log.Fields{
			"mode":          mode,
			"middleware":    d.Cfg.Middleware,
			"responseDelay": d.Cfg.ResponseDelay,
			"path":          req.URL.Path,
			"rawQuery":      req.URL.RawQuery,
			"method":        req.Method,
			"destination":   req.Host,
		}).Debug("Introducing response delay")

		time.Sleep(time.Duration(d.Cfg.ResponseDelay) * time.Millisecond)
	}

	return req, newResponse

}
