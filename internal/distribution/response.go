package distribution

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	ocierrors "github.com/mantle-sh/mantle/internal/distribution/errors"
)

// setContentDigest sets Docker-Content-Digest, which the specification requires
// on every manifest and blob response (REQ-OCI-03). Clients use it to verify
// what they received against what they asked for; omitting it silently disables
// that check.
func setContentDigest(w http.ResponseWriter, digest string) {
	w.Header().Set("Docker-Content-Digest", digest)
}

// writeJSON serialises a response body.
func writeJSON(w http.ResponseWriter, status int, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	w.WriteHeader(status)
	_, err = w.Write(encoded)
	return err
}

// paginationParams reads and bounds the `n` and `last` query parameters
// (REQ-OCI-08).
//
// n=0 is meaningful and must be honoured as "return nothing", which is why the
// presence of the parameter is distinguished from its value. Treating an
// explicit n=0 as "unset" and returning everything is a classic way to turn a
// probe into a full table scan.
func paginationParams(r *http.Request, defaultN, maxN int) (n int, last string, errs *ocierrors.Errors) {
	last = r.URL.Query().Get("last")
	raw := r.URL.Query().Get("n")
	if raw == "" {
		return defaultN, last, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0, "", ocierrors.WithDetail(ocierrors.CodeUnsupported,
			fmt.Sprintf("query parameter n must be a non-negative integer, got %q", raw),
			map[string]string{"n": raw})
	}
	if parsed > maxN {
		// Clamping rather than erroring: a client asking for more than the
		// instance will serve should get a page and a Link header, not a
		// failure it has no way to interpret.
		parsed = maxN
	}
	return parsed, last, nil
}

// setLinkHeader emits the RFC 5988 next-page link the specification requires
// when more results exist (REQ-OCI-08).
func setLinkHeader(w http.ResponseWriter, r *http.Request, n int, last string) {
	next := *r.URL
	query := next.Query()
	query.Set("n", strconv.Itoa(n))
	query.Set("last", last)
	next.Query()
	next.RawQuery = query.Encode()
	w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next.RequestURI()))
}

// requestIDFrom returns the correlation id assigned by middleware.
func requestIDFrom(r *http.Request) string {
	return r.Header.Get("X-Request-Id")
}
