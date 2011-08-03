// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

/*
Package multipart implements MIME multipart parsing, as defined in RFC
2046.

The implementation is sufficient for HTTP (RFC 2388) and the multipart
bodies generated by popular browsers.
*/
package multipart

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/textproto"
	"os"
	"regexp"
)

// TODO(bradfitz): inline these once the compiler can inline them in
// read-only situation (such as bytes.HasSuffix)
var lf = []byte("\n")
var crlf = []byte("\r\n")

var headerRegexp *regexp.Regexp = regexp.MustCompile("^([a-zA-Z0-9\\-]+): *([^\r\n]+)")

var emptyParams = make(map[string]string)

// A Part represents a single part in a multipart body.
type Part struct {
	// The headers of the body, if any, with the keys canonicalized
	// in the same fashion that the Go http.Request headers are.
	// i.e. "foo-bar" changes case to "Foo-Bar"
	Header textproto.MIMEHeader

	buffer *bytes.Buffer
	mr     *Reader

	disposition       string
	dispositionParams map[string]string
}

// FormName returns the name parameter if p has a Content-Disposition
// of type "form-data".  Otherwise it returns the empty string.
func (p *Part) FormName() string {
	// See http://tools.ietf.org/html/rfc2183 section 2 for EBNF
	// of Content-Disposition value format.
	if p.dispositionParams == nil {
		p.parseContentDisposition()
	}
	if p.disposition != "form-data" {
		return ""
	}
	return p.dispositionParams["name"]
}


// FileName returns the filename parameter of the Part's
// Content-Disposition header.
func (p *Part) FileName() string {
	if p.dispositionParams == nil {
		p.parseContentDisposition()
	}
	return p.dispositionParams["filename"]
}

func (p *Part) parseContentDisposition() {
	v := p.Header.Get("Content-Disposition")
	p.disposition, p.dispositionParams = mime.ParseMediaType(v)
	if p.dispositionParams == nil {
		p.dispositionParams = emptyParams
	}
}

// NewReader creates a new multipart Reader reading from r using the
// given MIME boundary.
func NewReader(reader io.Reader, boundary string) *Reader {
	b := []byte("\r\n--" + boundary + "--")
	return &Reader{
		bufReader: bufio.NewReader(reader),

		nl:               b[:2],
		nlDashBoundary:   b[:len(b)-2],
		dashBoundaryDash: b[2:],
		dashBoundary:     b[2 : len(b)-2],
	}
}

func newPart(mr *Reader) (*Part, os.Error) {
	bp := &Part{
		Header: make(map[string][]string),
		mr:     mr,
		buffer: new(bytes.Buffer),
	}
	if err := bp.populateHeaders(); err != nil {
		return nil, err
	}
	return bp, nil
}

func (bp *Part) populateHeaders() os.Error {
	for {
		lineBytes, err := bp.mr.bufReader.ReadSlice('\n')
		if err != nil {
			return err
		}
		line := string(lineBytes)
		if line == "\n" || line == "\r\n" {
			return nil
		}
		if matches := headerRegexp.FindStringSubmatch(line); len(matches) == 3 {
			bp.Header.Add(matches[1], matches[2])
			continue
		}
		return os.NewError("Unexpected header line found parsing multipart body")
	}
	panic("unreachable")
}

// Read reads the body of a part, after its headers and before the
// next part (if any) begins.
func (bp *Part) Read(p []byte) (n int, err os.Error) {
	if bp.buffer.Len() >= len(p) {
		// Internal buffer of unconsumed data is large enough for
		// the read request.  No need to parse more at the moment.
		return bp.buffer.Read(p)
	}
	peek, err := bp.mr.bufReader.Peek(4096) // TODO(bradfitz): add buffer size accessor
	unexpectedEof := err == os.EOF
	if err != nil && !unexpectedEof {
		return 0, fmt.Errorf("multipart: Part Read: %v", err)
	}
	if peek == nil {
		panic("nil peek buf")
	}

	// Search the peek buffer for "\r\n--boundary". If found,
	// consume everything up to the boundary. If not, consume only
	// as much of the peek buffer as cannot hold the boundary
	// string.
	nCopy := 0
	foundBoundary := false
	if idx := bytes.Index(peek, bp.mr.nlDashBoundary); idx != -1 {
		nCopy = idx
		foundBoundary = true
	} else if safeCount := len(peek) - len(bp.mr.nlDashBoundary); safeCount > 0 {
		nCopy = safeCount
	} else if unexpectedEof {
		// If we've run out of peek buffer and the boundary
		// wasn't found (and can't possibly fit), we must have
		// hit the end of the file unexpectedly.
		return 0, io.ErrUnexpectedEOF
	}
	if nCopy > 0 {
		if _, err := io.Copyn(bp.buffer, bp.mr.bufReader, int64(nCopy)); err != nil {
			return 0, err
		}
	}
	n, err = bp.buffer.Read(p)
	if err == os.EOF && !foundBoundary {
		// If the boundary hasn't been reached there's more to
		// read, so don't pass through an EOF from the buffer
		err = nil
	}
	return
}

func (bp *Part) Close() os.Error {
	io.Copy(ioutil.Discard, bp)
	return nil
}

// Reader is an iterator over parts in a MIME multipart body.
// Reader's underlying parser consumes its input as needed.  Seeking
// isn't supported.
type Reader struct {
	bufReader *bufio.Reader

	currentPart *Part
	partsRead   int

	nl, nlDashBoundary, dashBoundaryDash, dashBoundary []byte
}

// NextPart returns the next part in the multipart or an error.
// When there are no more parts, the error os.EOF is returned.
func (mr *Reader) NextPart() (*Part, os.Error) {
	if mr.currentPart != nil {
		mr.currentPart.Close()
	}

	expectNewPart := false
	for {
		line, err := mr.bufReader.ReadSlice('\n')
		if err != nil {
			return nil, fmt.Errorf("multipart: NextPart: %v", err)
		}

		if mr.isBoundaryDelimiterLine(line) {
			mr.partsRead++
			bp, err := newPart(mr)
			if err != nil {
				return nil, err
			}
			mr.currentPart = bp
			return bp, nil
		}

		if hasPrefixThenNewline(line, mr.dashBoundaryDash) {
			// Expected EOF
			return nil, os.EOF
		}

		if expectNewPart {
			return nil, fmt.Errorf("multipart: expecting a new Part; got line %q", string(line))
		}

		if mr.partsRead == 0 {
			// skip line
			continue
		}

		// Consume the "\n" or "\r\n" separator between the
		// body of the previous part and the boundary line we
		// now expect will follow. (either a new part or the
		// end boundary)
		if bytes.Equal(line, mr.nl) {
			expectNewPart = true
			continue
		}

		return nil, fmt.Errorf("multipart: unexpected line in Next(): %q", line)
	}
	panic("unreachable")
}

func (mr *Reader) isBoundaryDelimiterLine(line []byte) bool {
	// http://tools.ietf.org/html/rfc2046#section-5.1
	//   The boundary delimiter line is then defined as a line
	//   consisting entirely of two hyphen characters ("-",
	//   decimal value 45) followed by the boundary parameter
	//   value from the Content-Type header field, optional linear
	//   whitespace, and a terminating CRLF.
	if !bytes.HasPrefix(line, mr.dashBoundary) {
		return false
	}
	if bytes.HasSuffix(line, mr.nl) {
		return onlyHorizontalWhitespace(line[len(mr.dashBoundary) : len(line)-len(mr.nl)])
	}
	// Violate the spec and also support newlines without the
	// carriage return...
	if mr.partsRead == 0 && bytes.HasSuffix(line, lf) {
		if onlyHorizontalWhitespace(line[len(mr.dashBoundary) : len(line)-1]) {
			mr.nl = mr.nl[1:]
			mr.nlDashBoundary = mr.nlDashBoundary[1:]
			return true
		}
	}
	return false
}

func onlyHorizontalWhitespace(s []byte) bool {
	for _, b := range s {
		if b != ' ' && b != '\t' {
			return false
		}
	}
	return true
}

func hasPrefixThenNewline(s, prefix []byte) bool {
	return bytes.HasPrefix(s, prefix) &&
		(len(s) == len(prefix)+1 && s[len(s)-1] == '\n' ||
			len(s) == len(prefix)+2 && bytes.HasSuffix(s, crlf))
}
