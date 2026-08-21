package webrtc

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/protocols/whip"
)

//go:embed publish_index.html
var publishIndex []byte

//go:embed publisher.js
var publisherJS []byte

//go:embed read_index.html
var readIndex []byte

//go:embed reader.js
var readerJS []byte

var (
	reWHIPWHEPNoID   = regexp.MustCompile("^/(.+?)/(whip|whep)$")
	reWHIPWHEPWithID = regexp.MustCompile("^/(.+?)/(whip|whep)/(.+?)$")
)

func trailingSlashLocation(rawPath string, rawQuery string) string {
	res := path.Clean(rawPath)
	res = strings.TrimLeft(res, "/\\")
	res = "/" + res + "/"

	if rawQuery != "" {
		res += "?" + rawQuery
	}

	return res
}

func sessionLocation(publish bool, path string, rawQuery string, secret uuid.UUID) string {
	res := "/" + path + "/"

	if publish {
		res += "whip"
	} else {
		res += "whep"
	}

	res += "/" + secret.String()

	if rawQuery != "" {
		res += "?" + rawQuery
	}

	return res
}

type httpServer struct {
	address        string
	dumpPackets    bool
	encryption     bool
	serverKey      string
	serverCert     string
	allowOrigins   []string
	trustedProxies conf.IPNetworks
	readTimeout    conf.Duration
	writeTimeout   conf.Duration
	pathManager    serverPathManager
	parent         *Server

	inner *httpp.Server
}

func (s *httpServer) initialize() error {
	router := gin.New()
	router.SetTrustedProxies(s.trustedProxies.ToTrustedProxies()) //nolint:errcheck
	router.Use(s.middlewarePreflightRequests)

	// Recording API routes (before generic onRequest to avoid collision)
	if s.parent.RecMgr != nil {
		router.POST("/api/split-rec", func(c *gin.Context) {
			s.parent.SplitHandler.ServeHTTP(c)
		})
	}

	router.Use(s.onRequest)

	var proto string
	if s.encryption {
		proto = "webrtcs"
	} else {
		proto = "webrtc"
	}

	s.inner = &httpp.Server{
		Address:           s.address,
		AllowOrigins:      s.allowOrigins,
		DumpPackets:       s.dumpPackets,
		DumpPacketsPrefix: proto + "_server_conn",
		ReadTimeout:       time.Duration(s.readTimeout),
		WriteTimeout:      time.Duration(s.writeTimeout),
		Encryption:        s.encryption,
		ServerCert:        s.serverCert,
		ServerKey:         s.serverKey,
		Handler:           router,
		Parent:            s,
	}
	err := s.inner.Initialize()
	if err != nil {
		return err
	}

	return nil
}

// Log implements logger.Writer.
func (s *httpServer) Log(level logger.Level, format string, args ...any) {
	s.parent.Log(level, format, args...)
}

func (s *httpServer) close() {
	s.inner.Close()
}

func (s *httpServer) writeErrorNoLog(ctx *gin.Context, status int, err error) {
	ctx.AbortWithStatusJSON(status, &defs.APIError{
		Status: defs.APIErrorStatusError,
		Error:  err.Error(),
	})
}

func (s *httpServer) checkAuthOutsideSession(ctx *gin.Context, pathName string, publish bool) bool {
	_, err := s.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:        pathName,
			Query:       ctx.Request.URL.RawQuery,
			Publish:     publish,
			UserAgent:   ctx.Request.Header.Get("User-Agent"),
			Proto:       auth.ProtocolWebRTC,
			Credentials: httpp.Credentials(ctx.Request),
			IP:          net.ParseIP(ctx.ClientIP()),
		},
	})
	if err != nil {
		if terr, ok := errors.AsType[*auth.Error](err); ok {
			if terr.AskCredentials {
				ctx.Header("WWW-Authenticate", `Basic realm="mediamtx"`)
				s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
				return false
			}

			s.Log(logger.Info, "connection %v failed to authenticate: %v", httpp.RemoteAddr(ctx), terr.Wrapped)

			s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
			return false
		}

		s.writeErrorNoLog(ctx, http.StatusInternalServerError, err)
		return false
	}

	return true
}

func (s *httpServer) onWHIPOptions(ctx *gin.Context, pathName string, publish bool) {
	if !s.checkAuthOutsideSession(ctx, pathName, publish) {
		return
	}

	servers, err := s.parent.generateICEServers(true)
	if err != nil {
		s.writeErrorNoLog(ctx, http.StatusInternalServerError, err)
		return
	}

	ctx.Header("Access-Control-Allow-Methods", "OPTIONS, GET, POST, PATCH, DELETE")
	ctx.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match")
	ctx.Header("Access-Control-Expose-Headers", "Accept-Post, Link")
	ctx.Header("Accept-Post", "application/sdp")
	ctx.Writer.Header()["Link"] = whip.LinkHeaderMarshal(servers)
	ctx.Writer.WriteHeader(http.StatusNoContent)
}

// checkWHIPDeviceID authenticates a WHIP publish request as coming from
// ppobs. It decrypts the bearer token with the shared WHIP_AUTH_KEY
// (conf.WebRTCWHIPAuthKey / s.parent.WHIPAuthKey) - a token ppcenter issues
// only after verifying the ppobs app's own appId/appSecret credentials, see
// docs/obs-whip-publish-auth-protocol.md - and checks it hasn't expired and
// was issued for this exact path. This is separate from, and in addition
// to, checkAuthOutsideSession's username/password/IP checks.
//
// The canonical transport is the "WHIP-Device-Id" header. Stock WHIP clients
// have no way to send an arbitrary header - their only configurable
// credential is a "Bearer Token" field that goes out as a standard
// Authorization header - so "Authorization: Bearer <token>" is also
// accepted, as a fallback checked only when "WHIP-Device-Id" is absent, so
// it never collides with Basic/JWT credentials that a path's own publish
// auth might also carry in Authorization.
func (s *httpServer) checkWHIPDeviceID(ctx *gin.Context, pathName string) bool {
	token := ctx.Request.Header.Get("WHIP-Device-Id")
	if token == "" {
		if v := ctx.Request.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
			token = strings.TrimPrefix(v, "Bearer ")
		}
	}
	if token == "" {
		s.writeErrorNoLog(ctx, http.StatusForbidden, fmt.Errorf("publish deviceID authentication failure!"))
		return false
	}
	claims, err := decryptWHIPToken(s.parent.WHIPAuthKey, token, time.Now())
	if err != nil || claims.AppID+"/"+claims.Stream != pathName {
		s.writeErrorNoLog(ctx, http.StatusForbidden, fmt.Errorf("publish deviceID authentication failure!"))
		return false
	}
	return true
}

func (s *httpServer) onWHIPPost(ctx *gin.Context, pathName string, publish bool) {
	if publish && !s.checkWHIPDeviceID(ctx, pathName) {
		return
	}

	contentType := httpp.ParseContentType(ctx.Request.Header.Get("Content-Type"))
	if contentType != "application/sdp" {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid Content-Type"))
		return
	}

	offer, err := io.ReadAll(&customLimitReader{ctx.Request.Body, maxInboundSDPSize})
	if err != nil {
		return
	}

	res := s.parent.newSession(newSessionReq{
		pathName:    pathName,
		remoteAddr:  httpp.RemoteAddr(ctx),
		publish:     publish,
		offer:       offer,
		httpRequest: ctx.Request,
	})
	if res.err != nil {
		s.writeErrorNoLog(ctx, res.errStatusCode, res.err)
		return
	}

	res2 := res.sx.initialRequest(initialRequestReq{})
	if res2.err != nil {
		if terr, ok := errors.AsType[*auth.Error](res2.err); ok {
			if terr.AskCredentials {
				ctx.Header("WWW-Authenticate", `Basic realm="mediamtx"`)
				s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
				return
			}
			s.writeErrorNoLog(ctx, http.StatusUnauthorized, fmt.Errorf("authentication error"))
			return
		}

		s.writeErrorNoLog(ctx, res2.errStatusCode, res2.err)
		return
	}

	servers, err := s.parent.generateICEServers(true)
	if err != nil {
		s.writeErrorNoLog(ctx, http.StatusInternalServerError, err)
		return
	}

	ctx.Header("Content-Type", "application/sdp")
	ctx.Header("Access-Control-Expose-Headers", "ETag, ID, Accept-Patch, Link, Location")
	ctx.Header("ETag", "*")
	ctx.Header("ID", res.sx.uuid.String())

	// Accept-Patch has been removed from WHIP/WHEP specifications
	// but is kept here for compatibility reasons.
	ctx.Header("Accept-Patch", "application/trickle-ice-sdpfrag")

	ctx.Writer.Header()["Link"] = whip.LinkHeaderMarshal(servers)
	ctx.Header("Location", sessionLocation(publish, pathName, ctx.Request.URL.RawQuery, res.sx.secret))
	ctx.Writer.WriteHeader(http.StatusCreated)
	ctx.Writer.Write(res2.answer)
}

func (s *httpServer) onWHIPPatch(ctx *gin.Context, rawSecret string) {
	secret, err := uuid.Parse(rawSecret)
	if err != nil {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid secret"))
		return
	}

	contentType := httpp.ParseContentType(ctx.Request.Header.Get("Content-Type"))
	if contentType != "application/trickle-ice-sdpfrag" {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid Content-Type"))
		return
	}

	byts, err := io.ReadAll(&customLimitReader{ctx.Request.Body, maxInboundSDPSize})
	if err != nil {
		return
	}

	var frag whip.SDPFragment
	err = frag.Unmarshal(byts)
	if err != nil {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, err)
		return
	}

	res := s.parent.addSessionCandidates(addSessionCandidatesReq{
		secret:   secret,
		fragment: &frag,
	})
	if res.err != nil {
		if errors.Is(res.err, ErrSessionNotFound) {
			s.writeErrorNoLog(ctx, http.StatusNotFound, res.err)
		} else {
			s.writeErrorNoLog(ctx, http.StatusInternalServerError, res.err)
		}
		return
	}

	if res.answer != nil {
		var enc []byte
		enc, err = res.answer.Marshal()
		if err != nil {
			s.writeErrorNoLog(ctx, http.StatusInternalServerError, err)
			return
		}

		var ufrag string
		ufrag, _, err = sdpFragmentToCredentials(res.answer)
		if err != nil {
			s.writeErrorNoLog(ctx, http.StatusInternalServerError, err)
			return
		}

		ctx.Header("Content-Type", "application/trickle-ice-sdpfrag")
		ctx.Header("ETag", `"`+ufrag+`"`)
		ctx.Writer.WriteHeader(http.StatusOK)
		ctx.Writer.Write(enc) //nolint:errcheck
		return
	}

	ctx.AbortWithStatusJSON(http.StatusNoContent, &defs.APIOK{
		Status: defs.APIOKStatusOK,
	})
}

func (s *httpServer) onWHIPDelete(ctx *gin.Context, rawSecret string) {
	secret, err := uuid.Parse(rawSecret)
	if err != nil {
		s.writeErrorNoLog(ctx, http.StatusBadRequest, fmt.Errorf("invalid secret"))
		return
	}
	s.Log(logger.Info, "WHIP/WHEP session delete requested by %s, secret=%s, user-agent=%q",
		ctx.Request.RemoteAddr, rawSecret, ctx.Request.UserAgent())

	err = s.parent.deleteSession(deleteSessionReq{
		secret: secret,
	})
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			// DELETE is idempotent. After a server restart, WHIP clients can
			// retry deletion of a resource created by the previous process.
			// Returning 404 makes OBS stop its reconnect sequence.
			s.Log(logger.Info, "WHIP/WHEP session was already absent, accepting DELETE")
			ctx.AbortWithStatusJSON(http.StatusOK, &defs.APIOK{
				Status: defs.APIOKStatusOK,
			})
		} else {
			s.writeErrorNoLog(ctx, http.StatusInternalServerError, err)
		}
		return
	}

	ctx.AbortWithStatusJSON(http.StatusOK, &defs.APIOK{
		Status: defs.APIOKStatusOK,
	})
}

func (s *httpServer) onPage(ctx *gin.Context, pathName string, publish bool) {
	if !s.checkAuthOutsideSession(ctx, pathName, publish) {
		return
	}

	// Do not cache the HTML page.
	// This prevents a bug in Firefox in which, when the page
	// is loaded in an iframe and the iframe is deleted and recreated,
	// WebRTC is unable to re-establish the connection.
	ctx.Header("Cache-Control", "no-cache")
	ctx.Header("Content-Type", "text/html")
	ctx.Writer.WriteHeader(http.StatusOK)

	if publish {
		ctx.Writer.Write(publishIndex)
	} else {
		ctx.Writer.Write(readIndex)
	}
}

func (s *httpServer) middlewarePreflightRequests(ctx *gin.Context) {
	if ctx.Request.Method == http.MethodOptions &&
		ctx.Request.Header.Get("Access-Control-Request-Method") != "" {
		ctx.Header("Access-Control-Allow-Methods", "OPTIONS, GET, POST, PATCH, DELETE")
		ctx.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match")
		ctx.AbortWithStatus(http.StatusNoContent)
		return
	}
}

func (s *httpServer) onRequest(ctx *gin.Context) {
	// WHIP degrade-protocol control WebSocket (see
	// docs/obs-mmx-degrade-protocol.md). Must be checked before
	// reWHIPWHEPNoID below: a path ending in ".../ws/whip" would otherwise
	// also match it (which only requires the last segment to be literally
	// "whip"), misrouting this as a WHIP publish attempt for path ".../ws".
	if s.parent.DegradeWSPathSuffix != "" &&
		ctx.Request.Method == http.MethodGet &&
		strings.HasSuffix(ctx.Request.URL.Path, s.parent.DegradeWSPathSuffix) &&
		len(ctx.Request.URL.Path) > len(s.parent.DegradeWSPathSuffix)+1 {
		pathName := strings.TrimSuffix(ctx.Request.URL.Path, s.parent.DegradeWSPathSuffix)
		pathName = strings.TrimPrefix(pathName, "/")
		s.handleWHIPDegradeWebSocket(ctx, pathName)
		return
	}

	// WHIP/WHEP, outside session
	if m := reWHIPWHEPNoID.FindStringSubmatch(ctx.Request.URL.Path); m != nil {
		switch ctx.Request.Method {
		case http.MethodOptions:
			s.onWHIPOptions(ctx, m[1], m[2] == "whip")

		case http.MethodPost:
			s.onWHIPPost(ctx, m[1], m[2] == "whip")

		case http.MethodGet, http.MethodHead, http.MethodPut:
			// RFC draft-ietf-whip-09
			// The WHIP endpoints MUST return an "405 Method Not Allowed" response
			// for any HTTP GET, HEAD or PUT requests
			s.writeErrorNoLog(ctx, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		}
		return
	}

	// WHIP/WHEP, inside session
	if m := reWHIPWHEPWithID.FindStringSubmatch(ctx.Request.URL.Path); m != nil {
		switch ctx.Request.Method {
		case http.MethodPatch:
			s.onWHIPPatch(ctx, m[3])

		case http.MethodDelete:
			s.onWHIPDelete(ctx, m[3])
		}
		return
	}

	// WHEP media and ABR control endpoint.
	if ctx.Request.URL.Path == s.parent.ABRWSPath &&
		ctx.Request.Method == http.MethodGet {
		s.handleABRWebSocket(ctx)
		return
	}

	// static resources
	if ctx.Request.Method == http.MethodGet {
		switch {
		case strings.HasSuffix(ctx.Request.URL.Path, "/publisher.js"):
			ctx.Header("Cache-Control", "max-age=3600")
			ctx.Header("Content-Type", "application/javascript")
			ctx.Writer.WriteHeader(http.StatusOK)
			ctx.Writer.Write(publisherJS)

		case strings.HasSuffix(ctx.Request.URL.Path, "/reader.js"):
			ctx.Header("Cache-Control", "max-age=3600")
			ctx.Header("Content-Type", "application/javascript")
			ctx.Writer.WriteHeader(http.StatusOK)
			ctx.Writer.Write(readerJS)

		case ctx.Request.URL.Path == "/favicon.ico":

		case len(ctx.Request.URL.Path) >= 2:
			switch {
			case len(ctx.Request.URL.Path) > len("/publish") && strings.HasSuffix(ctx.Request.URL.Path, "/publish"):
				s.onPage(ctx, ctx.Request.URL.Path[1:len(ctx.Request.URL.Path)-len("/publish")], true)

			case ctx.Request.URL.Path[len(ctx.Request.URL.Path)-1] != '/':
				ctx.Header("Location", trailingSlashLocation(ctx.Request.URL.Path, ctx.Request.URL.RawQuery))
				ctx.Writer.WriteHeader(http.StatusFound)

			default:
				s.onPage(ctx, ctx.Request.URL.Path[1:len(ctx.Request.URL.Path)-1], false)
			}
		}
	}
}
