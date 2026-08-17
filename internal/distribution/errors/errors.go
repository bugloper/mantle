// Package errors implements the OCI Distribution error envelope (REQ-OCI-10)
// and the code-to-status table from Appendix B of the specification.
//
// Every error leaving the /v2 surface must be shaped by this package. Handlers
// never write an error body by hand: a bare string body is a conformance bug
// that only shows up in somebody else's client.
package errors

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Code is an OCI error code. The set is closed — Appendix B is the whole list.
type Code string

const (
	// CodeUnknown is the generic server-fault code. It is not in the Appendix B
	// table because it is not a client error: it exists so that an internal
	// failure still reaches the client inside the OCI envelope with a 500,
	// rather than as HTML or a bare string that no client can parse.
	CodeUnknown Code = "UNKNOWN"

	CodeBlobUnknown         Code = "BLOB_UNKNOWN"
	CodeBlobUploadInvalid   Code = "BLOB_UPLOAD_INVALID"
	CodeBlobUploadUnknown   Code = "BLOB_UPLOAD_UNKNOWN"
	CodeDigestInvalid       Code = "DIGEST_INVALID"
	CodeManifestBlobUnknown Code = "MANIFEST_BLOB_UNKNOWN"
	CodeManifestInvalid     Code = "MANIFEST_INVALID"
	CodeManifestUnknown     Code = "MANIFEST_UNKNOWN"
	CodeNameInvalid         Code = "NAME_INVALID"
	CodeNameUnknown         Code = "NAME_UNKNOWN"
	CodeSizeInvalid         Code = "SIZE_INVALID"
	CodeUnauthorized        Code = "UNAUTHORIZED"
	CodeDenied              Code = "DENIED"
	CodeUnsupported         Code = "UNSUPPORTED"
	CodeTooManyRequests     Code = "TOOMANYREQUESTS"
)

// status maps each code to its HTTP status. Appendix B.
var status = map[Code]int{
	CodeUnknown:             http.StatusInternalServerError,
	CodeBlobUnknown:         http.StatusNotFound,
	CodeBlobUploadInvalid:   http.StatusBadRequest,
	CodeBlobUploadUnknown:   http.StatusNotFound,
	CodeDigestInvalid:       http.StatusBadRequest,
	CodeManifestBlobUnknown: http.StatusNotFound,
	CodeManifestInvalid:     http.StatusBadRequest,
	CodeManifestUnknown:     http.StatusNotFound,
	CodeNameInvalid:         http.StatusBadRequest,
	CodeNameUnknown:         http.StatusNotFound,
	CodeSizeInvalid:         http.StatusBadRequest,
	CodeUnauthorized:        http.StatusUnauthorized,
	CodeDenied:              http.StatusForbidden,
	CodeUnsupported:         http.StatusBadRequest,
	CodeTooManyRequests:     http.StatusTooManyRequests,
}

// Status returns the HTTP status for a code, defaulting to 500 for codes that
// somehow escaped the table.
func (c Code) Status() int {
	if s, ok := status[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Error is a single entry in the OCI error array.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errors is the envelope: {"errors":[…]}. Multiple errors are permitted by the
// spec, and manifest validation genuinely produces several at once.
type Errors struct {
	Errors []*Error `json:"errors"`
}

func (e *Errors) Error() string {
	if len(e.Errors) == 0 {
		return "unknown error"
	}
	if len(e.Errors) == 1 {
		return e.Errors[0].Error()
	}
	return fmt.Sprintf("%s (and %d more)", e.Errors[0].Error(), len(e.Errors)-1)
}

// Status reports the HTTP status for the envelope, which is the status of the
// first error. Mixing codes with different statuses in one envelope is a bug in
// the caller, not something to resolve here.
func (e *Errors) Status() int {
	if len(e.Errors) == 0 {
		return http.StatusInternalServerError
	}
	return e.Errors[0].Code.Status()
}

// New builds a single-error envelope.
func New(code Code, message string) *Errors {
	return &Errors{Errors: []*Error{{Code: code, Message: message}}}
}

// WithDetail builds a single-error envelope carrying a detail object. Detail is
// where the digest, tag, or repository name that caused the failure belongs —
// clients surface it, and it is the difference between a debuggable failure and
// "manifest unknown".
func WithDetail(code Code, message string, detail any) *Errors {
	return &Errors{Errors: []*Error{{Code: code, Message: message, Detail: detail}}}
}

// Append adds an error to an existing envelope, allocating if needed.
func (e *Errors) Append(code Code, message string, detail any) *Errors {
	if e == nil {
		e = &Errors{}
	}
	e.Errors = append(e.Errors, &Error{Code: code, Message: message, Detail: detail})
	return e
}

// Standard envelopes for the cases that recur across handlers. Constructing
// these in one place keeps the wording consistent, which matters because
// clients match on it more than anyone would like.
func BlobUnknown(digest string) *Errors {
	return WithDetail(CodeBlobUnknown, "blob unknown to registry", map[string]string{"Digest": digest})
}

func ManifestUnknown(reference string) *Errors {
	return WithDetail(CodeManifestUnknown, "manifest unknown", map[string]string{"Tag": reference})
}

func NameUnknown(name string) *Errors {
	return WithDetail(CodeNameUnknown, "repository name not known to registry", map[string]string{"Name": name})
}

func NameInvalid(name, reason string) *Errors {
	return WithDetail(CodeNameInvalid, "invalid repository name: "+reason, map[string]string{"Name": name})
}

func DigestInvalid(reason string) *Errors {
	return New(CodeDigestInvalid, "provided digest did not match uploaded content: "+reason)
}

func ManifestBlobUnknown(digest string) *Errors {
	return WithDetail(CodeManifestBlobUnknown, "manifest references a blob unknown to registry",
		map[string]string{"Digest": digest})
}

func UploadUnknown(ref string) *Errors {
	return WithDetail(CodeBlobUploadUnknown, "blob upload unknown to registry", map[string]string{"Reference": ref})
}

func Denied(reason string) *Errors {
	return New(CodeDenied, reason)
}

func Unsupported(reason string) *Errors {
	return New(CodeUnsupported, reason)
}

// ServeJSON writes an error envelope with the correct status and content type.
// extraHeaders is used for the WWW-Authenticate challenge on 401 and
// Retry-After on 429, both of which the spec requires alongside the body.
func ServeJSON(w http.ResponseWriter, errs *Errors, extraHeaders http.Header) {
	for k, vs := range extraHeaders {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	body, err := json.Marshal(errs)
	if err != nil {
		// Marshalling a fixed-shape struct cannot realistically fail, but a
		// caller-supplied Detail could contain something unmarshalable.
		body = []byte(`{"errors":[{"code":"UNSUPPORTED","message":"error serialization failed"}]}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(errs.Status())
	_, _ = w.Write(body)
}

// As extracts an *Errors from an error chain, so that internal layers can
// return typed OCI errors and handlers can pass them straight to the client
// without a translation table in every handler.
func As(err error) (*Errors, bool) {
	var e *Errors
	if errors.As(err, &e) {
		return e, true
	}
	var single *Error
	if errors.As(err, &single) {
		return &Errors{Errors: []*Error{single}}, true
	}
	return nil, false
}
