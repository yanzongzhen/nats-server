// Copyright 2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"compress/flate"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type wsOpCode int

const (
	// From https://tools.ietf.org/html/rfc6455#section-5.2
	wsTextMessage   = wsOpCode(1)
	wsBinaryMessage = wsOpCode(2)
	wsCloseMessage  = wsOpCode(8)
	wsPingMessage   = wsOpCode(9)
	wsPongMessage   = wsOpCode(10)

	wsFinalBit = 1 << 7
	wsRsv1Bit  = 1 << 6 // Used for compression, from https://tools.ietf.org/html/rfc7692#section-6
	wsRsv2Bit  = 1 << 5
	wsRsv3Bit  = 1 << 4

	wsMaskBit = 1 << 7

	wsContinuationFrame     = 0
	wsMaxFrameHeaderSize    = 10 // For a server-to-client frame
	wsMaxControlPayloadSize = 125
	wsFrameSizeForBrowsers  = 4096 // From experiment, webrowsers behave better with limited frame size

	// From https://tools.ietf.org/html/rfc6455#section-11.7
	wsCloseStatusNormalClosure      = 1000
	wsCloseStatusGoingAway          = 1001
	wsCloseStatusProtocolError      = 1002
	wsCloseStatusUnsupportedData    = 1003
	wsCloseStatusNoStatusReceived   = 1005
	wsCloseStatusAbnormalClosure    = 1006
	wsCloseStatusInvalidPayloadData = 1007
	wsCloseStatusPolicyViolation    = 1008
	wsCloseStatusMessageTooBig      = 1009
	wsCloseStatusInternalSrvError   = 1011
	wsCloseStatusTLSHandshake       = 1015

	wsFirstFrame        = true
	wsContFrame         = false
	wsFinalFrame        = true
	wsCompressedFrame   = true
	wsUncompressedFrame = false
)

var decompressorPool sync.Pool

// From https://tools.ietf.org/html/rfc6455#section-1.3
var wsGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

type websocket struct {
	frames     net.Buffers
	fs         int64
	closeMsg   []byte
	compress   bool
	closeSent  bool
	browser    bool
	compressor *flate.Writer
	cookieJwt  string
}

type srvWebsocket struct {
	mu             sync.RWMutex
	server         *http.Server
	listener       net.Listener
	tls            bool
	allowedOrigins map[string]*allowedOrigin // host will be the key
	sameOrigin     bool
	connectURLs    []string
	connectURLsMap refCountedUrlSet
	users          map[string]*User
	nkeys          map[string]*NkeyUser
	authOverride   bool // indicate if there is auth override in websocket config
}

type allowedOrigin struct {
	scheme string
	port   string
}

type wsUpgradeResult struct {
	conn net.Conn
	ws   *websocket
}

type wsReadInfo struct {
	rem   int
	fs    bool
	ff    bool
	fc    bool
	mkpos byte
	mkey  [4]byte
	buf   []byte
}

func (r *wsReadInfo) init() {
	r.fs, r.ff = true, true
}

// Returns a slice containing `needed` bytes from the given buffer `buf`
// starting at position `pos`, and possibly read from the given reader `r`.
// When bytes are present in `buf`, the `pos` is incremented by the number
// of bytes found up to `needed` and the new position is returned. If not
// enough bytes are found, the bytes found in `buf` are copied to the returned
// slice and the remaning bytes are read from `r`.
func wsGet(r io.Reader, buf []byte, pos, needed int) ([]byte, int, error) {
	avail := len(buf) - pos
	if avail >= needed {
		return buf[pos : pos+needed], pos + needed, nil
	}
	b := make([]byte, needed)
	start := copy(b, buf[pos:])
	for start != needed {
		n, err := r.Read(b[start:cap(b)])
		if err != nil {
			return nil, 0, err
		}
		start += n
	}
	return b, pos + avail, nil
}

// Returns a slice of byte slices corresponding to payload of websocket frames.
// The byte slice `buf` is filled with bytes from the connection's read loop.
// This function will decode the frame headers and unmask the payload(s).
// It is possible that the returned slices point to the given `buf` slice, so
// `buf` should not be overwritten until the returned slices have been parsed.
//
// Client lock MUST NOT be held on entry.
func (c *client) wsRead(r *wsReadInfo, ior io.Reader, buf []byte) ([][]byte, error) {
	var (
		bufs   [][]byte
		tmpBuf []byte
		err    error
		pos    int
		max    = len(buf)
	)
	for pos != max {
		if r.fs {
			b0 := buf[pos]
			frameType := wsOpCode(b0 & 0xF)
			final := b0&wsFinalBit != 0
			compressed := b0&wsRsv1Bit != 0
			pos++

			tmpBuf, pos, err = wsGet(ior, buf, pos, 1)
			if err != nil {
				return bufs, err
			}
			b1 := tmpBuf[0]

			// Clients MUST set the mask bit. If not set, reject.
			if b1&wsMaskBit == 0 {
				return bufs, c.wsHandleProtocolError("mask bit missing")
			}

			// Store size in case it is < 125
			r.rem = int(b1 & 0x7F)

			switch frameType {
			case wsPingMessage, wsPongMessage, wsCloseMessage:
				if r.rem > wsMaxControlPayloadSize {
					return bufs, c.wsHandleProtocolError(
						fmt.Sprintf("control frame length bigger than maximum allowed of %v bytes",
							wsMaxControlPayloadSize))
				}
				if !final {
					return bufs, c.wsHandleProtocolError("control frame does not have final bit set")
				}
			case wsTextMessage, wsBinaryMessage:
				if !r.ff {
					return bufs, c.wsHandleProtocolError("new message started before final frame for previous message was received")
				}
				r.ff = final
				r.fc = compressed
			case wsContinuationFrame:
				// Compressed bit must be only set in the first frame
				if r.ff || compressed {
					return bufs, c.wsHandleProtocolError("invalid continuation frame")
				}
				r.ff = final
			default:
				return bufs, c.wsHandleProtocolError(fmt.Sprintf("unknown opcode %v", frameType))
			}

			switch r.rem {
			case 126:
				tmpBuf, pos, err = wsGet(ior, buf, pos, 2)
				if err != nil {
					return bufs, err
				}
				r.rem = int(binary.BigEndian.Uint16(tmpBuf))
			case 127:
				tmpBuf, pos, err = wsGet(ior, buf, pos, 8)
				if err != nil {
					return bufs, err
				}
				r.rem = int(binary.BigEndian.Uint64(tmpBuf))
			}

			// Read masking key
			tmpBuf, pos, err = wsGet(ior, buf, pos, 4)
			if err != nil {
				return bufs, err
			}
			copy(r.mkey[:], tmpBuf)
			r.mkpos = 0

			// Handle control messages in place...
			if wsIsControlFrame(frameType) {
				pos, err = c.wsHandleControlFrame(r, frameType, ior, buf, pos)
				if err != nil {
					return bufs, err
				}
				continue
			}

			// Done with the frame header
			r.fs = false
		}
		if pos < max {
			var b []byte
			var n int

			n = r.rem
			if pos+n > max {
				n = max - pos
			}
			b = buf[pos : pos+n]
			pos += n
			r.rem -= n
			if r.fc {
				r.buf = append(r.buf, b...)
				b = r.buf
			}
			if !r.fc || r.rem == 0 {
				r.unmask(b)
				if r.fc {
					// As per https://tools.ietf.org/html/rfc7692#section-7.2.2
					// add 0x00, 0x00, 0xff, 0xff and then a final block so that flate reader
					// does not report unexpected EOF.
					b = append(b, 0x00, 0x00, 0xff, 0xff, 0x01, 0x00, 0x00, 0xff, 0xff)
					br := bytes.NewBuffer(b)
					d, _ := decompressorPool.Get().(io.ReadCloser)
					if d == nil {
						d = flate.NewReader(br)
					} else {
						d.(flate.Resetter).Reset(br, nil)
					}
					b, err = ioutil.ReadAll(d)
					decompressorPool.Put(d)
					if err != nil {
						return bufs, err
					}
				}
				bufs = append(bufs, b)
				if r.rem == 0 {
					r.fs, r.fc, r.buf = true, false, nil
				}
			}
		}
	}
	return bufs, nil
}

// Handles the PING, PONG and CLOSE websocket control frames.
//
// Client lock MUST NOT be held on entry.
func (c *client) wsHandleControlFrame(r *wsReadInfo, frameType wsOpCode, nc io.Reader, buf []byte, pos int) (int, error) {
	var payload []byte
	var err error

	statusPos := pos
	if r.rem > 0 {
		payload, pos, err = wsGet(nc, buf, pos, r.rem)
		if err != nil {
			return pos, err
		}
		r.unmask(payload)
		r.rem = 0
	}
	switch frameType {
	case wsCloseMessage:
		status := wsCloseStatusNoStatusReceived
		body := ""
		// If there is a payload, it should contain 2 unsigned bytes
		// that represent the status code and then optional payload.
		if len(payload) >= 2 {
			status = int(binary.BigEndian.Uint16(buf[statusPos : statusPos+2]))
			body = string(buf[statusPos+2 : statusPos+len(payload)])
			if body != "" && !utf8.ValidString(body) {
				// https://tools.ietf.org/html/rfc6455#section-5.5.1
				// If body is present, it must be a valid utf8
				status = wsCloseStatusInvalidPayloadData
				body = "invalid utf8 body in close frame"
			}
		}
		c.wsEnqueueControlMessage(wsCloseMessage, wsCreateCloseMessage(status, body))
		// Return io.EOF so that readLoop will close the connection as ClientClosed
		// after processing pending buffers.
		return pos, io.EOF
	case wsPingMessage:
		c.wsEnqueueControlMessage(wsPongMessage, payload)
	case wsPongMessage:
		// Nothing to do..
	}
	return pos, nil
}

// Unmask the given slice.
func (r *wsReadInfo) unmask(buf []byte) {
	p := int(r.mkpos)
	if len(buf) < 16 {
		for i := 0; i < len(buf); i++ {
			buf[i] ^= r.mkey[p&3]
			p++
		}
		r.mkpos = byte(p & 3)
		return
	}
	var k [8]byte
	for i := 0; i < 8; i++ {
		k[i] = r.mkey[(p+i)&3]
	}
	km := binary.BigEndian.Uint64(k[:])
	n := (len(buf) / 8) * 8
	for i := 0; i < n; i += 8 {
		tmp := binary.BigEndian.Uint64(buf[i : i+8])
		tmp ^= km
		binary.BigEndian.PutUint64(buf[i:], tmp)
	}
	buf = buf[n:]
	for i := 0; i < len(buf); i++ {
		buf[i] ^= r.mkey[p&3]
		p++
	}
	r.mkpos = byte(p & 3)
}

// Returns true if the op code corresponds to a control frame.
func wsIsControlFrame(frameType wsOpCode) bool {
	return frameType >= wsCloseMessage
}

// Create the frame header.
// Encodes the frame type and optional compression flag, and the size of the payload.
func wsCreateFrameHeader(compressed bool, frameType wsOpCode, l int) []byte {
	fh := make([]byte, wsMaxFrameHeaderSize)
	n := wsFillFrameHeader(fh, wsFirstFrame, wsFinalFrame, compressed, frameType, l)
	return fh[:n]
}

func wsFillFrameHeader(fh []byte, first, final, compressed bool, frameType wsOpCode, l int) int {
	var n int
	var b byte
	if first {
		b = byte(frameType)
	}
	if final {
		b |= wsFinalBit
	}
	if compressed {
		b |= wsRsv1Bit
	}
	switch {
	case l <= 125:
		n = 2
		fh[0] = b
		fh[1] = byte(l)
	case l < 65536:
		n = 4
		fh[0] = b
		fh[1] = 126
		binary.BigEndian.PutUint16(fh[2:], uint16(l))
	default:
		n = 10
		fh[0] = b
		fh[1] = 127
		binary.BigEndian.PutUint64(fh[2:], uint64(l))
	}
	return n
}

// Invokes wsEnqueueControlMessageLocked under client lock.
//
// Client lock MUST NOT be held on entry
func (c *client) wsEnqueueControlMessage(controlMsg wsOpCode, payload []byte) {
	c.mu.Lock()
	c.wsEnqueueControlMessageLocked(controlMsg, payload)
	c.mu.Unlock()
}

// Enqueues a websocket control message.
// If the control message is a wsCloseMessage, then marks this client
// has having sent the close message (since only one should be sent).
// This will prevent the generic closeConnection() to enqueue one.
//
// Client lock held on entry.
func (c *client) wsEnqueueControlMessageLocked(controlMsg wsOpCode, payload []byte) {
	// Control messages are never compressed and their size will be
	// less than wsMaxControlPayloadSize, which means the frame header
	// will be only 2 bytes.
	cm := make([]byte, 2+len(payload))
	wsFillFrameHeader(cm, wsFirstFrame, wsFinalFrame, wsUncompressedFrame, controlMsg, len(payload))
	// Note that payload is optional.
	if len(payload) > 0 {
		copy(cm[2:], payload)
	}
	c.out.pb += int64(len(cm))
	if controlMsg == wsCloseMessage {
		// We can't add the close message to the frames buffers
		// now. It will be done on a flushOutbound() when there
		// are no more pending buffers to send.
		c.ws.closeSent = true
		c.ws.closeMsg = cm
	} else {
		c.ws.frames = append(c.ws.frames, cm)
		c.ws.fs += int64(len(cm))
	}
	c.flushSignal()
}

// Enqueues a websocket close message with a status mapped from the given `reason`.
//
// Client lock held on entry
func (c *client) wsEnqueueCloseMessage(reason ClosedState) {
	var status int
	switch reason {
	case ClientClosed:
		status = wsCloseStatusNormalClosure
	case AuthenticationTimeout, AuthenticationViolation, SlowConsumerPendingBytes, SlowConsumerWriteDeadline,
		MaxAccountConnectionsExceeded, MaxConnectionsExceeded, MaxControlLineExceeded, MaxSubscriptionsExceeded,
		MissingAccount, AuthenticationExpired, Revocation:
		status = wsCloseStatusPolicyViolation
	case TLSHandshakeError:
		status = wsCloseStatusTLSHandshake
	case ParseError, ProtocolViolation, BadClientProtocolVersion:
		status = wsCloseStatusProtocolError
	case MaxPayloadExceeded:
		status = wsCloseStatusMessageTooBig
	case ServerShutdown:
		status = wsCloseStatusGoingAway
	case WriteError, ReadError, StaleConnection:
		status = wsCloseStatusAbnormalClosure
	default:
		status = wsCloseStatusInternalSrvError
	}
	body := wsCreateCloseMessage(status, reason.String())
	c.wsEnqueueControlMessageLocked(wsCloseMessage, body)
}

// Create and then enqueue a close message with a protocol error and the
// given message. This is invoked when parsing websocket frames.
//
// Lock MUST NOT be held on entry.
func (c *client) wsHandleProtocolError(message string) error {
	buf := wsCreateCloseMessage(wsCloseStatusProtocolError, message)
	c.wsEnqueueControlMessage(wsCloseMessage, buf)
	return fmt.Errorf(message)
}

// Create a close message with the given `status` and `body`.
// If the `body` is more than the maximum allows control frame payload size,
// it is truncated and "..." is added at the end (as a hint that message
// is not complete).
func wsCreateCloseMessage(status int, body string) []byte {
	// Since a control message payload is limited in size, we
	// will limit the text and add trailing "..." if truncated.
	// The body of a Close Message must be preceded with 2 bytes,
	// so take that into account for limiting the body length.
	if len(body) > wsMaxControlPayloadSize-2 {
		body = body[:wsMaxControlPayloadSize-5]
		body += "..."
	}
	buf := make([]byte, 2+len(body))
	// We need to have a 2 byte unsigned int that represents the error status code
	// https://tools.ietf.org/html/rfc6455#section-5.5.1
	binary.BigEndian.PutUint16(buf[:2], uint16(status))
	copy(buf[2:], []byte(body))
	return buf
}

// Process websocket client handshake. On success, returns the raw net.Conn that
// will be used to create a *client object.
// Invoked from the HTTP server listening on websocket port.
func (s *Server) wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsUpgradeResult, error) {
	opts := s.getOpts()

	// From https://tools.ietf.org/html/rfc6455#section-4.2.1
	// Point 1.
	if r.Method != "GET" {
		return nil, wsReturnHTTPError(w, http.StatusMethodNotAllowed, "request method must be GET")
	}
	// Point 2.
	if r.Host == "" {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "'Host' missing in request")
	}
	// Point 3.
	if !wsHeaderContains(r.Header, "Upgrade", "websocket") {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "invalid value for header 'Uprade'")
	}
	// Point 4.
	if !wsHeaderContains(r.Header, "Connection", "Upgrade") {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "invalid value for header 'Connection'")
	}
	// Point 5.
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "key missing")
	}
	// Point 6.
	if !wsHeaderContains(r.Header, "Sec-Websocket-Version", "13") {
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "invalid version")
	}
	// Others are optional
	// Point 7.
	if err := s.websocket.checkOrigin(r); err != nil {
		return nil, wsReturnHTTPError(w, http.StatusForbidden, fmt.Sprintf("origin not allowed: %v", err))
	}
	// Point 8.
	// We don't have protocols, so ignore.
	// Point 9.
	// Extensions, only support for compression at the moment
	compress := opts.Websocket.Compression
	if compress {
		compress = wsClientSupportsCompression(r.Header)
	}

	h := w.(http.Hijacker)
	conn, brw, err := h.Hijack()
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		return nil, wsReturnHTTPError(w, http.StatusInternalServerError, err.Error())
	}
	if brw.Reader.Buffered() > 0 {
		conn.Close()
		return nil, wsReturnHTTPError(w, http.StatusBadRequest, "client sent data before handshake is complete")
	}

	var buf [1024]byte
	p := buf[:0]

	// From https://tools.ietf.org/html/rfc6455#section-4.2.2
	p = append(p, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "...)
	p = append(p, wsAcceptKey(key)...)
	p = append(p, _CRLF_...)
	if compress {
		p = append(p, "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"...)
	}
	p = append(p, _CRLF_...)

	if _, err = conn.Write(p); err != nil {
		conn.Close()
		return nil, err
	}
	// If there was a deadline set for the handshake, clear it now.
	if opts.Websocket.HandshakeTimeout > 0 {
		conn.SetDeadline(time.Time{})
	}
	ws := &websocket{compress: compress}
	// Indicate if this is likely coming from a browser.
	if ua := r.Header.Get("User-Agent"); ua != "" && strings.HasPrefix(ua, "Mozilla/") {
		ws.browser = true
	}
	if opts.Websocket.JWTCookie != "" {
		if c, err := r.Cookie(opts.Websocket.JWTCookie); err == nil && c != nil {
			ws.cookieJwt = c.Value
		}
	}
	return &wsUpgradeResult{conn: conn, ws: ws}, nil
}

// Returns true if the header named `name` contains a token with value `value`.
func wsHeaderContains(header http.Header, name string, value string) bool {
	for _, s := range header[name] {
		tokens := strings.Split(s, ",")
		for _, t := range tokens {
			t = strings.Trim(t, " \t")
			if strings.EqualFold(t, value) {
				return true
			}
		}
	}
	return false
}

// Return true if the client has "permessage-deflate" in its extensions.
func wsClientSupportsCompression(header http.Header) bool {
	for _, extensionList := range header["Sec-Websocket-Extensions"] {
		extensions := strings.Split(extensionList, ",")
		for _, extension := range extensions {
			extension = strings.Trim(extension, " \t")
			params := strings.Split(extension, ";")
			for _, p := range params {
				p = strings.Trim(p, " \t")
				if strings.EqualFold(p, "permessage-deflate") {
					return true
				}
			}
		}
	}
	return false
}

// Send an HTTP error with the given `status`` to the given http response writer `w`.
// Return an error created based on the `reason` string.
func wsReturnHTTPError(w http.ResponseWriter, status int, reason string) error {
	err := fmt.Errorf("websocket handshake error: %s", reason)
	w.Header().Set("Sec-Websocket-Version", "13")
	http.Error(w, http.StatusText(status), status)
	return err
}

// If the server is configured to accept any origin, then this function returns
// `nil` without checking if the Origin is present and valid.
// Otherwise, this will check that the Origin matches the same origine or
// any origin in the allowed list.
func (w *srvWebsocket) checkOrigin(r *http.Request) error {
	w.mu.RLock()
	checkSame := w.sameOrigin
	listEmpty := len(w.allowedOrigins) == 0
	w.mu.RUnlock()
	if !checkSame && listEmpty {
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Sec-Websocket-Origin")
	}
	if origin == "" {
		return errors.New("origin not provided")
	}
	u, err := url.ParseRequestURI(origin)
	if err != nil {
		return err
	}
	oh, op, err := wsGetHostAndPort(u.Scheme == "https", u.Host)
	if err != nil {
		return err
	}
	// If checking same origin, compare with the http's request's Host.
	if checkSame {
		rh, rp, err := wsGetHostAndPort(r.TLS != nil, r.Host)
		if err != nil {
			return err
		}
		if oh != rh || op != rp {
			return errors.New("not same origin")
		}
		// I guess it is possible to have cases where one wants to check
		// same origin, but also that the origin is in the allowed list.
		// So continue with the next check.
	}
	if !listEmpty {
		w.mu.RLock()
		ao := w.allowedOrigins[oh]
		w.mu.RUnlock()
		if ao == nil || u.Scheme != ao.scheme || op != ao.port {
			return errors.New("not in the allowed list")
		}
	}
	return nil
}

func wsGetHostAndPort(tls bool, hostport string) (string, string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// If error is missing port, then use defaults based on the scheme
		if ae, ok := err.(*net.AddrError); ok && strings.Contains(ae.Err, "missing port") {
			err = nil
			host = hostport
			if tls {
				port = "443"
			} else {
				port = "80"
			}
		}
	}
	return strings.ToLower(host), port, err
}

// Concatenate the key sent by the client with the GUID, then computes the SHA1 hash
// and returns it as a based64 encoded string.
func wsAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write(wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Validate the websocket related options.
func validateWebsocketOptions(o *Options) error {
	wo := &o.Websocket
	// If no port is defined, we don't care about other options
	if wo.Port == 0 {
		return nil
	}
	// Enforce TLS... unless NoTLS is set to true.
	if wo.TLSConfig == nil && !wo.NoTLS {
		return errors.New("websocket requires TLS configuration")
	}
	// Make sure that allowed origins, if specified, can be parsed.
	for _, ao := range wo.AllowedOrigins {
		if _, err := url.Parse(ao); err != nil {
			return fmt.Errorf("unable to parse allowed origin: %v", err)
		}
	}
	// If there is a NoAuthUser, we need to have Users defined and
	// the user to be present.
	if wo.NoAuthUser != _EMPTY_ {
		if wo.Users == nil {
			return fmt.Errorf("websocket no_auth_user %q configured, but users are not", wo.NoAuthUser)
		}
		for _, u := range wo.Users {
			if u.Username == wo.NoAuthUser {
				return nil
			}
		}
		return fmt.Errorf("websocket no_auth_user %q not found in users configuration", wo.NoAuthUser)
	}
	// Using JWT requires Trusted Keys
	if wo.JWTCookie != "" {
		if len(o.TrustedOperators) == 0 && len(o.TrustedKeys) == 0 {
			return fmt.Errorf("trusted operators or trusted keys configuration is required for JWT authentication via cookie %q", wo.JWTCookie)
		}
	}
	return nil
}

// Creates or updates the existing map
func (s *Server) wsSetOriginOptions(o *WebsocketOpts) {
	ws := &s.websocket
	ws.mu.Lock()
	defer ws.mu.Unlock()
	// Copy over the option's same origin boolean
	ws.sameOrigin = o.SameOrigin
	// Reset the map. Will help for config reload if/when we support it.
	ws.allowedOrigins = nil
	if o.AllowedOrigins == nil {
		return
	}
	for _, ao := range o.AllowedOrigins {
		// We have previously checked (during options validation) that the urls
		// are parseable, but if we get an error, report and skip.
		u, err := url.ParseRequestURI(ao)
		if err != nil {
			s.Errorf("error parsing allowed origin: %v", err)
			continue
		}
		h, p, _ := wsGetHostAndPort(u.Scheme == "https", u.Host)
		if ws.allowedOrigins == nil {
			ws.allowedOrigins = make(map[string]*allowedOrigin, len(o.AllowedOrigins))
		}
		ws.allowedOrigins[h] = &allowedOrigin{scheme: u.Scheme, port: p}
	}
}

// Given the websocket options, we check if any auth configuration
// has been provided. If so, possibly create users/nkey users and
// store them in s.websocket.users/nkeys.
// Also update a boolean that indicates if auth is required for
// websocket clients.
// Server lock is held on entry.
func (s *Server) wsConfigAuth(opts *WebsocketOpts) {
	ws := &s.websocket
	if len(opts.Nkeys) > 0 || len(opts.Users) > 0 {
		ws.nkeys, ws.users = s.buildNkeysAndUsersFromOptions(opts.Nkeys, opts.Users)
		ws.authOverride = true
	} else if opts.Username != "" || opts.Token != "" {
		ws.authOverride = true
	} else {
		ws.users = nil
		ws.nkeys = nil
		ws.authOverride = false
	}
}

func (s *Server) startWebsocketServer() {
	sopts := s.getOpts()
	o := &sopts.Websocket

	s.wsSetOriginOptions(o)

	var hl net.Listener
	var proto string
	var err error

	port := o.Port
	if port == -1 {
		port = 0
	}
	hp := net.JoinHostPort(o.Host, strconv.Itoa(port))

	// We are enforcing (when validating the options) the use of TLS, but the
	// code was originally supporting both modes. The reason for TLS only is
	// that we expect users to send JWTs with bearer tokens and we want to
	// avoid the possibility of it being "intercepted".

	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return
	}
	// Do not check o.NoTLS here. If a TLS configuration is available, use it,
	// regardless of NoTLS. If we don't have a TLS config, it means that the
	// user has configured NoTLS because otherwise the server would have failed
	// to start due to options validation.
	if o.TLSConfig != nil {
		proto = "wss"
		config := o.TLSConfig.Clone()
		hl, err = tls.Listen("tcp", hp, config)
	} else {
		proto = "ws"
		hl, err = net.Listen("tcp", hp)
	}
	if err != nil {
		s.mu.Unlock()
		s.Fatalf("Unable to listen for websocket connections: %v", err)
		return
	}
	s.Noticef("Listening for websocket clients on %s://%s:%d", proto, o.Host, port)
	if proto == "ws" {
		s.Warnf("Websocket not configured with TLS. DO NOT USE IN PRODUCTION!")
	}

	s.websocket.tls = proto == "wss"
	if port == 0 {
		s.opts.Websocket.Port = hl.Addr().(*net.TCPAddr).Port
	}
	s.websocket.connectURLs, err = s.getConnectURLs(o.Advertise, o.Host, o.Port)
	if err != nil {
		s.Fatalf("Unable to get websocket connect URLs: %v", err)
		hl.Close()
		s.mu.Unlock()
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.wsUpgrade(w, r)
		if err != nil {
			s.Errorf(err.Error())
			return
		}
		s.createClient(res.conn, res.ws)
	})
	hs := &http.Server{
		Addr:        hp,
		Handler:     mux,
		ReadTimeout: o.HandshakeTimeout,
		ErrorLog:    log.New(&wsCaptureHTTPServerLog{s}, "", 0),
	}
	s.websocket.server = hs
	s.websocket.listener = hl
	go func() {
		if err := hs.Serve(hl); err != http.ErrServerClosed {
			s.Fatalf("websocket listener error: %v", err)
		}
		if s.isLameDuckMode() {
			// Signal that we are not accepting new clients
			s.ldmCh <- true
			// Now wait for the Shutdown...
			<-s.quitCh
			return
		}
		s.done <- true
	}()
	s.mu.Unlock()
}

type wsCaptureHTTPServerLog struct {
	s *Server
}

func (cl *wsCaptureHTTPServerLog) Write(p []byte) (int, error) {
	var buf [128]byte
	var b = buf[:0]

	copy(b, []byte("websocket :"))
	offset := 0
	if bytes.HasPrefix(p, []byte("http:")) {
		offset = 6
	}
	b = append(b, p[offset:]...)
	cl.s.Errorf(string(b))
	return len(p), nil
}

func (c *client) wsCollapsePtoNB() (net.Buffers, int64) {
	var nb net.Buffers
	var total = 0
	var mfs = 0
	if c.ws.browser {
		mfs = wsFrameSizeForBrowsers
	}
	if len(c.out.p) > 0 {
		p := c.out.p
		c.out.p = nil
		nb = append(c.out.nb, p)
	} else if len(c.out.nb) > 0 {
		nb = c.out.nb
	}
	// Start with possible already framed buffers (that we could have
	// got from partials or control messages such as ws pings or pongs).
	bufs := c.ws.frames
	if c.ws.compress && len(nb) > 0 {
		buf := &bytes.Buffer{}

		cp := c.ws.compressor
		if cp == nil {
			c.ws.compressor, _ = flate.NewWriter(buf, flate.BestSpeed)
			cp = c.ws.compressor
		} else {
			cp.Reset(buf)
		}
		var usz int
		var csz int
		for _, b := range nb {
			usz += len(b)
			cp.Write(b)
		}
		cp.Close()
		b := buf.Bytes()
		p := b[:len(b)-4]
		if mfs > 0 && len(p) > mfs {
			for first, final := true, false; len(p) > 0; first = false {
				lp := len(p)
				if lp > mfs {
					lp = mfs
				} else {
					final = true
				}
				fh := make([]byte, wsMaxFrameHeaderSize)
				n := wsFillFrameHeader(fh, first, final, wsCompressedFrame, wsBinaryMessage, lp)
				bufs = append(bufs, fh[:n], p[:lp])
				csz += n + lp
				p = p[lp:]
			}
		} else {
			h := wsCreateFrameHeader(true, wsBinaryMessage, len(p))
			bufs = append(bufs, h, p)
			csz = len(h) + len(p)
		}
		// Add to pb the compressed data size (including headers), but
		// remove the original uncompressed data size that was added
		// during the queueing.
		c.out.pb += int64(csz) - int64(usz)
		c.ws.fs += int64(csz)
	} else if len(nb) > 0 {
		if mfs > 0 {
			// We are limiting the frame size.
			startFrame := func() int {
				bufs = append(bufs, make([]byte, wsMaxFrameHeaderSize))
				return len(bufs) - 1
			}
			endFrame := func(idx, size int) {
				n := wsFillFrameHeader(bufs[idx], wsFirstFrame, wsFinalFrame, wsUncompressedFrame, wsBinaryMessage, size)
				c.out.pb += int64(n)
				c.ws.fs += int64(n + size)
				bufs[idx] = bufs[idx][:n]
			}

			fhIdx := startFrame()
			for i := 0; i < len(nb); i++ {
				b := nb[i]
				if total+len(b) <= mfs {
					bufs = append(bufs, b)
					total += len(b)
					continue
				}
				for len(b) > 0 {
					endFrame(fhIdx, total)
					total = len(b)
					if total >= mfs {
						total = mfs
					}
					fhIdx = startFrame()
					bufs = append(bufs, b[:total])
					b = b[total:]
				}
			}
			if total > 0 {
				endFrame(fhIdx, total)
			}
		} else {
			// If there is no limit on the frame size, create a single frame for
			// all pending buffers.
			for _, b := range nb {
				total += len(b)
			}
			wsfh := wsCreateFrameHeader(false, wsBinaryMessage, total)
			c.out.pb += int64(len(wsfh))
			bufs = append(bufs, wsfh)
			bufs = append(bufs, nb...)
			c.ws.fs += int64(len(wsfh) + total)
		}
	}
	if len(c.ws.closeMsg) > 0 {
		bufs = append(bufs, c.ws.closeMsg)
		c.ws.fs += int64(len(c.ws.closeMsg))
		c.ws.closeMsg = nil
	}
	c.ws.frames = nil
	return bufs, c.ws.fs
}
