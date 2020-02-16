// Copyright 2012 Junqing Tan <ivan@mysqlab.net> and The Go Authors
// Use of this source code is governed by a BSD-style
// Part of source code is from Go fcgi package

package fcgiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/atomic"
)

const FCGI_LISTENSOCK_FILENO uint8 = 0
const FCGI_HEADER_LEN uint8 = 8
const VERSION_1 uint8 = 1
const FCGI_NULL_REQUEST_ID uint8 = 0
const FCGI_KEEP_CONN uint8 = 1
const doubleCRLF = "\r\n\r\n"

const (
	FCGI_BEGIN_REQUEST uint8 = iota + 1
	FCGI_ABORT_REQUEST
	FCGI_END_REQUEST
	FCGI_PARAMS
	FCGI_STDIN
	FCGI_STDOUT
	FCGI_STDERR
	FCGI_DATA
	FCGI_GET_VALUES
	FCGI_GET_VALUES_RESULT
	FCGI_UNKNOWN_TYPE
	FCGI_MAXTYPE = FCGI_UNKNOWN_TYPE
)

const (
	FCGI_RESPONDER uint8 = iota + 1
	FCGI_AUTHORIZER
	FCGI_FILTER
)

const (
	FCGI_REQUEST_COMPLETE uint8 = iota
	FCGI_CANT_MPX_CONN
	FCGI_OVERLOADED
	FCGI_UNKNOWN_ROLE
)

const (
	FCGI_MAX_CONNS  string = "MAX_CONNS"
	FCGI_MAX_REQS   string = "MAX_REQS"
	FCGI_MPXS_CONNS string = "MPXS_CONNS"
)

const (
	maxWrite = 65500 // 65530 may work, but for compatibility
	maxPad   = 255
)

type header struct {
	Version       uint8
	Type          uint8
	Id            uint16
	ContentLength uint16
	PaddingLength uint8
	Reserved      uint8
}

// for padding so we don't have to allocate all the time
// not synchronized because we don't care what the contents are
var pad [maxPad]byte

func (h *header) init(recType uint8, reqId uint16, contentLength int) {
	h.Version = 1
	h.Type = recType
	h.Id = reqId
	h.ContentLength = uint16(contentLength)
	h.PaddingLength = uint8(-contentLength & 7)
}

type record struct {
	h    header
	rbuf []byte
}

func (rec *record) read(r io.Reader) (buf []byte, err error) {
	if err = binary.Read(r, binary.BigEndian, &rec.h); err != nil {
		return
	}
	if rec.h.Version != 1 {
		err = errors.New("fcgi: invalid header version")
		return
	}
	if rec.h.Type == FCGI_END_REQUEST {
		err = io.EOF
		return
	}
	n := int(rec.h.ContentLength) + int(rec.h.PaddingLength)
	if len(rec.rbuf) < n {
		rec.rbuf = make([]byte, n)
	}
	if n, err = io.ReadFull(r, rec.rbuf[:n]); err != nil {
		return
	}
	buf = rec.rbuf[:int(rec.h.ContentLength)]

	return
}

type FCGIClient struct {
	mutex     sync.Mutex
	rwc       io.ReadWriteCloser
	h         header
	buf       bytes.Buffer
	keepAlive bool
	reqId     *atomic.Int32
}

// DialContext connects to the fcgi responder at the specified network address.
// See func net.Dial for a description of the network and address parameters.
func DialContext(ctx context.Context, network, address string) (fcgi *FCGIClient, err error) {
	var (
		conn net.Conn
		dial net.Dialer
	)

	if conn, err = dial.DialContext(ctx, network, address);err != nil {
		return
	}

	fcgi = &FCGIClient{
		rwc:       conn,
		reqId:     atomic.NewInt32(1),
	}

	return
}
// Connects to the fcgi responder at the specified network address.
// See func net.Dial for a description of the network and address parameters.
func Dial(network, address string) (*FCGIClient, error) {
	return DialContext(context.Background(), network, address)
}

// Connects to the fcgi responder at the specified network address with timeout
// See func net.DialTimeout for a description of the network, address and timeout parameters.
func DialTimeout(network, address string, timeout time.Duration) (*FCGIClient,  error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return DialContext(ctx, network, address)
}

// Close fcgi connnection
func (c *FCGIClient) Close() error { return c.rwc.Close() }

func (c *FCGIClient) writeRecord(id uint16, recType uint8, content []byte) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.buf.Reset()

	c.h.init(recType, id, len(content))
	if err := binary.Write(&c.buf, binary.BigEndian, c.h); err != nil {
		return err
	}
	if _, err := c.buf.Write(content); err != nil {
		return err
	}
	if _, err := c.buf.Write(pad[:c.h.PaddingLength]); err != nil {
		return err
	}
	_, err = c.rwc.Write(c.buf.Bytes())
	return err
}

func (c *FCGIClient) writeBeginRequest(id, role uint16, flags uint8) error {
	b := [8]byte{byte(role >> 8), byte(role), flags}
	return c.writeRecord(id, FCGI_BEGIN_REQUEST, b[:])
}

func (c *FCGIClient) writeEndRequest(id uint16, appStatus int, protocolStatus uint8) error {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(appStatus))
	b[4] = protocolStatus
	return c.writeRecord(id, FCGI_END_REQUEST, b)
}

func (c *FCGIClient) writePairs(id uint16, recType uint8, pairs map[string]string) error {
	w := newWriter(id, c, recType)
	b := make([]byte, 8)
	nn := 0
	for k, v := range pairs {
		m := 8 + len(k) + len(v)
		if m > maxWrite {
			// param data size exceed 65535 bytes"
			vl := maxWrite - 8 - len(k)
			v = v[:vl]
		}
		n := encodeSize(b, uint32(len(k)))
		n += encodeSize(b[n:], uint32(len(v)))
		m = n + len(k) + len(v)
		if (nn + m) > maxWrite {
			w.Flush()
			nn = 0
		}
		nn += m
		if _, err := w.Write(b[:n]); err != nil {
			return err
		}
		if _, err := w.WriteString(k); err != nil {
			return err
		}
		if _, err := w.WriteString(v); err != nil {
			return err
		}
	}
	return w.Close()
}

func encodeSize(b []byte, size uint32) int {
	if size > 127 {
		size |= 1 << 31
		binary.BigEndian.PutUint32(b, size)
		return 4
	}
	b[0] = byte(size)
	return 1
}

// bufWriter encapsulates bufio.Writer but also closes the underlying stream when
// Closed.
type bufWriter struct {
	closer io.Closer
	*bufio.Writer
}

func (w *bufWriter) Close() error {
	if err := w.Writer.Flush(); err != nil {
		_ = w.closer.Close()

		return err
	}
	return w.closer.Close()
}

func newWriter(id uint16, c *FCGIClient, recType uint8) *bufWriter {
	s := &streamWriter{id: id, c: c, recType: recType}
	w := bufio.NewWriterSize(s, maxWrite)
	return &bufWriter{s, w}
}

// streamWriter abstracts out the separation of a stream into discrete records.
// It only writes maxWrite bytes at a time.
type streamWriter struct {
	c       *FCGIClient
	id      uint16
	recType uint8
}

func (w *streamWriter) Write(p []byte) (int, error) {
	nn := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxWrite {
			n = maxWrite
		}
		if err := w.c.writeRecord(w.id, w.recType, p[:n]); err != nil {
			return nn, err
		}
		nn += n
		p = p[n:]
	}
	return nn, nil
}

func (w *streamWriter) Close() error {
	// send empty record to close the stream
	return w.c.writeRecord(w.id, w.recType, nil)
}

type streamReader struct {
	c   *FCGIClient
	buf []byte
}

func (w *streamReader) Read(p []byte) (n int, err error) {

	if len(p) > 0 {
		if len(w.buf) == 0 {
			rec := &record{}
			w.buf, err = rec.read(w.c.rwc)
			if err != nil {
				return
			}
		}

		n = len(p)
		if n > len(w.buf) {
			n = len(w.buf)
		}
		copy(p, w.buf[:n])
		w.buf = w.buf[n:]
	}

	return
}

func (c *FCGIClient) Listen(ctx context.Context) {
	done := ctx.Done()

	for {
		select {
		case <-done:
			return
		default:
			// read connection data
		}

		r := &streamReader{c: c}

		rb := bufio.NewReader(r)
		tp := textproto.NewReader(rb)
		resp := new(http.Response)
		// Parse the first line of the response.
		line, err := tp.ReadLine()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}

			// cb(resp, err)

			continue
		}

		if i := strings.IndexByte(line, ' '); i == -1 {
			err = &badStringError{"malformed HTTP response", line}
		} else {
			resp.Proto = line[:i]
			resp.Status = strings.TrimLeft(line[i+1:], " ")
		}

		statusCode := resp.Status
		if i := strings.IndexByte(resp.Status, ' '); i != -1 {
			statusCode = resp.Status[:i]
		}
		if len(statusCode) != 3 {
			err = &badStringError{"malformed HTTP status code", statusCode}
		}
		resp.StatusCode, err = strconv.Atoi(statusCode)
		if err != nil || resp.StatusCode < 0 {
			err = &badStringError{"malformed HTTP status code", statusCode}
		}
		var ok bool
		if resp.ProtoMajor, resp.ProtoMinor, ok = http.ParseHTTPVersion(resp.Proto); !ok {
			err = &badStringError{"malformed HTTP version", resp.Proto}
		}
		// Parse the response headers.
		mimeHeader, err := tp.ReadMIMEHeader()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}

			// cb(resp, err)

			continue
		}

		resp.Header = http.Header(mimeHeader)
		// TODO: fixTransferEncoding ?
		resp.TransferEncoding = resp.Header["Transfer-Encoding"]
		resp.ContentLength, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

		if chunked(resp.TransferEncoding) {
			resp.Body = ioutil.NopCloser(httputil.NewChunkedReader(rb))
		} else {
			resp.Body = ioutil.NopCloser(rb)
		}

		// cb(resp, err)
	}
}

// Do made the request and returns a io.Reader that translates the data read
// from fcgi responder out of fcgi packet before returning it.
func (c *FCGIClient) Do(p map[string]string, req io.Reader) (r io.Reader, err error) {
	c.mutex.Lock()
	id := c.reqId.Inc()
	if id >= math.MaxInt16 {
		id = 1
		c.reqId.Store(id)
	}
	c.mutex.Unlock()

	err = c.writeBeginRequest(uint16(id), uint16(FCGI_RESPONDER), 0)
	if err != nil {
		return
	}

	err = c.writePairs(uint16(id), FCGI_PARAMS, p)
	if err != nil {
		return
	}

	body := newWriter(uint16(id), c, FCGI_STDIN)
	if req != nil {
		io.Copy(body, req)
	}
	body.Close()

	r = &streamReader{c: c}
	return
}

type badStringError struct {
	what string
	str  string
}

func (e *badStringError) Error() string { return fmt.Sprintf("%s %q", e.what, e.str) }

// Request returns a HTTP Response with Header and Body
// from fcgi responder
func (c *FCGIClient) Request(p map[string]string, req io.Reader) (resp *http.Response, err error) {

	r, err := c.Do(p, req)
	if err != nil {
		return
	}

	rb := bufio.NewReader(r)
	tp := textproto.NewReader(rb)
	resp = new(http.Response)
	// Parse the first line of the response.
	line, err := tp.ReadLine()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if i := strings.IndexByte(line, ' '); i == -1 {
		err = &badStringError{"malformed HTTP response", line}
	} else {
		resp.Proto = line[:i]
		resp.Status = strings.TrimLeft(line[i+1:], " ")
	}
	statusCode := resp.Status
	if i := strings.IndexByte(resp.Status, ' '); i != -1 {
		statusCode = resp.Status[:i]
	}
	if len(statusCode) != 3 {
		err = &badStringError{"malformed HTTP status code", statusCode}
	}
	resp.StatusCode, err = strconv.Atoi(statusCode)
	if err != nil || resp.StatusCode < 0 {
		err = &badStringError{"malformed HTTP status code", statusCode}
	}
	var ok bool
	if resp.ProtoMajor, resp.ProtoMinor, ok = http.ParseHTTPVersion(resp.Proto); !ok {
		err = &badStringError{"malformed HTTP version", resp.Proto}
	}
	// Parse the response headers.
	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	resp.Header = http.Header(mimeHeader)
	// TODO: fixTransferEncoding ?
	resp.TransferEncoding = resp.Header["Transfer-Encoding"]
	resp.ContentLength, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

	if chunked(resp.TransferEncoding) {
		resp.Body = ioutil.NopCloser(httputil.NewChunkedReader(rb))
	} else {
		resp.Body = ioutil.NopCloser(rb)
	}
	return
}

// Get issues a GET request to the fcgi responder.
func (c *FCGIClient) Get(p map[string]string) (resp *http.Response, err error) {

	p["REQUEST_METHOD"] = "GET"
	p["CONTENT_LENGTH"] = "0"

	return c.Request(p, nil)
}

// Get issues a Post request to the fcgi responder. with request body
// in the format that bodyType specified
func (c *FCGIClient) Post(p map[string]string, bodyType string, body io.Reader, l int) (resp *http.Response, err error) {

	if len(p["REQUEST_METHOD"]) == 0 || p["REQUEST_METHOD"] == "GET" {
		p["REQUEST_METHOD"] = "POST"
	}
	p["CONTENT_LENGTH"] = strconv.Itoa(l)
	if len(bodyType) > 0 {
		p["CONTENT_TYPE"] = bodyType
	} else {
		p["CONTENT_TYPE"] = "application/x-www-form-urlencoded"
	}

	return c.Request(p, body)
}

// PostForm issues a POST to the fcgi responder, with form
// as a string key to a list values (url.Values)
func (c *FCGIClient) PostForm(p map[string]string, data url.Values) (resp *http.Response, err error) {
	body := bytes.NewReader([]byte(data.Encode()))
	return c.Post(p, "application/x-www-form-urlencoded", body, body.Len())
}

// PostFile issues a POST to the fcgi responder in multipart(RFC 2046) standard,
// with form as a string key to a list values (url.Values),
// and/or with file as a string key to a list file path.
func (c *FCGIClient) PostFile(p map[string]string, data url.Values, file map[string]string) (resp *http.Response, err error) {
	buf := &bytes.Buffer{}
	writer := multipart.NewWriter(buf)
	bodyType := writer.FormDataContentType()

	for key, val := range data {
		for _, v0 := range val {
			err = writer.WriteField(key, v0)
			if err != nil {
				return
			}
		}
	}

	for key, val := range file {
		fd, e := os.Open(val)
		if e != nil {
			return nil, e
		}
		defer fd.Close()

		part, e := writer.CreateFormFile(key, filepath.Base(val))
		if e != nil {
			return nil, e
		}
		_, err = io.Copy(part, fd)
	}

	err = writer.Close()
	if err != nil {
		return
	}

	return c.Post(p, bodyType, buf, buf.Len())
}

// Checks whether chunked is part of the encodings stack
func chunked(te []string) bool { return len(te) > 0 && te[0] == "chunked" }
