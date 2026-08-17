// Package events defines the registry's internal event vocabulary and the sink
// interface that carries it to the ledger, the audit log, and webhooks (§7.2).
//
// It exists so that the distribution layer can emit events without importing
// the things that consume them. The §7.2 dependency rule is that nothing
// imports `distribution`, and that ledger, gc, retention, and admin never
// appear in its import graph — so both sides depend on this package instead.
// That is what keeps a product feature from becoming a dependency of the pull
// path (principle 2, NG-1), and it is enforced by a test in test/architecture.
package events

import "time"

// Pull is one observed read.
type Pull struct {
	RepositoryID int64
	ManifestID   *int64
	Reference    string
	Digest       string
	IdentityID   *int64
	Address      string
	UserAgent    string
	// Kind is "manifest" or "blob". The distinction drives Tier 0 deployment
	// inference: a manifest read followed by layer reads is a deployment,
	// whereas a manifest read alone is an inspection.
	Kind       string
	OccurredAt time.Time
}

// Kinds of pull event.
const (
	KindManifest = "manifest"
	KindBlob     = "blob"
)

// Push is one completed write.
type Push struct {
	RepositoryID   int64
	ManifestID     int64
	Digest         string
	Tag            string
	MediaType      string
	IdentityID     *int64
	ConfigDigest   string
	Annotations    map[string]string
	OccurredAt     time.Time
	TagMoved       bool
	PreviousDigest string
}

// Sink receives registry events.
//
// Both methods must return immediately and must never block the caller. A sink
// that cannot keep up drops events; a sink that slows a pull turns an analytics
// feature into a production incident (REQ-LEDGER-01, REQ-LEDGER-02).
type Sink interface {
	RecordPull(Pull)
	RecordPush(Push)
}

// Discard is a sink that drops everything, for tests and for an instance
// running with the ledger disabled.
type Discard struct{}

func (Discard) RecordPull(Pull) {}
func (Discard) RecordPush(Push) {}

// Multi fans an event out to several sinks.
//
// Delivery is sequential and best-effort. Every sink is expected to be
// non-blocking, so fanning out in the caller's goroutine costs a handful of
// channel sends; spawning a goroutine per event would cost more than the work.
type Multi []Sink

func (m Multi) RecordPull(e Pull) {
	for _, sink := range m {
		sink.RecordPull(e)
	}
}

func (m Multi) RecordPush(e Push) {
	for _, sink := range m {
		sink.RecordPush(e)
	}
}
