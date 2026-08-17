package ledger

import (
	"sync"
	"time"
)

// clientKey identifies a client pulling from one repository. It is the join key
// for Tier 0 inference: a manifest pull and the layer pulls that follow it come
// from the same client, and that correlation is the entire signal.
type clientKey struct {
	repositoryID int64
	identityID   int64
	address      string
}

// manifestObservation is a manifest pull awaiting corroborating layer pulls.
type manifestObservation struct {
	manifestID int64
	reference  string
	observedAt time.Time
	layerPulls int
	// emitted prevents one manifest pull from producing a deployment record per
	// subsequent layer, which on a fifty-layer image would be fifty writes.
	emitted bool
}

// inferenceTracker correlates manifest pulls with the layer pulls that follow.
//
// The state is intentionally in-memory and per-node. Inference is a
// best-effort signal, and persisting it would put a database write back on the
// pull path — the exact thing REQ-LEDGER-01 forbids. On a multi-node
// deployment a client's manifest and layer pulls may land on different nodes
// and the inference is simply missed; that is an acceptable loss for a Tier 0
// signal, and Tier 1 reporting is the answer for anyone who needs certainty.
type inferenceTracker struct {
	mu     sync.Mutex
	window time.Duration
	recent map[clientKey]*manifestObservation

	// maxEntries bounds memory. A registry serving many clients must not
	// accumulate an unbounded map because of a feature that is allowed to be
	// approximate.
	maxEntries int
}

func newInferenceTracker(window time.Duration) *inferenceTracker {
	return &inferenceTracker{
		window:     window,
		recent:     make(map[clientKey]*manifestObservation),
		maxEntries: 10000,
	}
}

// observeManifest records a manifest pull, starting a new correlation window.
func (t *inferenceTracker) observeManifest(key clientKey, manifestID int64, reference string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Shed the whole map rather than evict cleverly if it has grown too large.
	// Losing inference state degrades a best-effort signal; an LRU here would
	// be more code and more locking for no product benefit.
	if len(t.recent) >= t.maxEntries {
		t.recent = make(map[clientKey]*manifestObservation)
	}

	t.recent[key] = &manifestObservation{
		manifestID: manifestID,
		reference:  reference,
		observedAt: now,
	}
}

// observeLayer records a layer pull and reports whether the correlated manifest
// pull now looks like a deployment.
func (t *inferenceTracker) observeLayer(key clientKey, now time.Time, minLayers int) (manifestObservation, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	observation, ok := t.recent[key]
	if !ok {
		return manifestObservation{}, false
	}
	if now.Sub(observation.observedAt) > t.window {
		delete(t.recent, key)
		return manifestObservation{}, false
	}
	if observation.emitted {
		return manifestObservation{}, false
	}

	observation.layerPulls++
	if observation.layerPulls < minLayers {
		return manifestObservation{}, false
	}

	observation.emitted = true
	return *observation, true
}

// expire drops correlations older than the window.
func (t *inferenceTracker) expire(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, observation := range t.recent {
		if now.Sub(observation.observedAt) > t.window {
			delete(t.recent, key)
		}
	}
}
